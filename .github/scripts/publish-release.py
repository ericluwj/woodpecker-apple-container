"""Publish immutable revisions and refresh the matching rolling GitHub Release."""
import json
import os
import re
import subprocess
import tempfile
import time
from pathlib import Path
from urllib.parse import quote

ASSETS = ("woodpecker-agent-apple-darwin-arm64.tar.gz", "SHA256SUMS")


class GitHubError(RuntimeError):
    def __init__(self, status, message):
        self.status = status
        super().__init__(message)


def gh(*args, binary=False):
    try:
        output = subprocess.check_output(["gh", *args], stderr=subprocess.PIPE)
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.decode(errors="replace")
        match = re.search(r"HTTP (\d{3})", detail)
        status = int(match[1]) if match else None
        if status in (401, 403):
            raise GitHubError(status, "GitHub denied publishing access. Check contents:write and repository tag rules; historical workflow-bearing commits may require an administrator-provided token with workflow permissions.") from exc
        raise GitHubError(status, detail.strip() or "GitHub CLI request failed") from exc
    return output if binary else output.decode().strip()


def api(path, method="GET", **fields):
    args = ["api", f"repos/{os.environ['GH_REPO']}/{path}", "--method", method]
    for key, value in fields.items():
        if isinstance(value, bool):
            args += ["-F", f"{key}={str(value).lower()}"]
        else:
            args += ["-f", f"{key}={value}"]
    return json.loads(gh(*args) or "null")


def visible_read(read):
    # Retry only reads: repeating a create/upload after a timeout can duplicate
    # resources. Authorization errors must remain actionable and fail promptly.
    for attempt in range(5):
        try:
            return read()
        except GitHubError as exc:
            if exc.status != 404 or attempt == 4:
                raise
            time.sleep(2 ** attempt)


def create_release(tag, notes):
    # The response contains the authoritative ID even before list/tag indexes
    # expose the draft. Tags have already been created and ownership checked.
    created = api("releases", "POST", tag_name=tag, name=tag, body=notes,
                  draft=True, prerelease=False, make_latest="false")
    return visible_read(lambda: api(f"releases/{created['id']}"))


def upload_asset(current, name, path, replace=False):
    existing = next((a for a in current["assets"] if a["name"] == name), None)
    if existing is not None:
        if not replace:
            if download_asset(existing) != path.read_bytes():
                raise RuntimeError(f"Refusing to replace immutable asset {current['tag_name']}/{name}")
            return
        api(f"releases/assets/{existing['id']}", "DELETE")
    url = current["upload_url"].split("{")[0] + "?name=" + quote(name, safe="")
    asset = json.loads(gh("api", url, "--method", "POST", "--header",
                          "Content-Type: application/octet-stream", "--input", str(path)))
    current["assets"] = [a for a in current["assets"] if a["name"] != name] + [asset]


def download_asset(asset):
    return visible_read(lambda: gh("api", f"repos/{os.environ['GH_REPO']}/releases/assets/{asset['id']}",
                                   "--header", "Accept: application/octet-stream", binary=True))


def publish_release(current, notes=None):
    fields = dict(draft=False, prerelease=False, make_latest="false")
    if notes is not None:
        fields["body"] = notes
    api(f"releases/{current['id']}", "PATCH", **fields)
    current["draft"] = False
    current["prerelease"] = False


def release(tag):
    # Listing also distinguishes an absent release from an authentication error.
    pages = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/releases?per_page=100",
                          "--paginate", "--slurp"))
    return next((r for page in pages for r in page if r["tag_name"] == tag), None)


def ref_sha(tag):
    # Resolve lightweight or annotated tags to their commit.
    data = api(f"git/ref/tags/{tag}")["object"]
    while data["type"] == "tag":
        data = api(f"git/tags/{data['sha']}")["object"]
    if data["type"] != "commit":
        raise RuntimeError("Release tag must point to a commit")
    return data["sha"]


def publish(tag, sha, dist):
    match = re.fullmatch(r"(\d+\.\d+\.\d+)-([1-9]\d*)", tag)
    if not match:
        raise ValueError("Expected a numbered release, e.g. 3.20.0-1")
    base = match[1]
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("Expected a full source commit SHA")
    subprocess.run(["shasum", "-a", "256", "-c", "SHA256SUMS"], cwd=dist, check=True)
    current = release(tag)
    if current is None:
        # Fail rather than repoint an existing tag at a different commit.
        refs = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/git/matching-refs/tags/{tag}"))
        if any(r["ref"] == f"refs/tags/{tag}" for r in refs):
            if ref_sha(tag) != sha:
                raise RuntimeError("Existing revision tag points to a different commit")
        else:
            api("git/refs", "POST", ref=f"refs/tags/{tag}", sha=sha)
        current = create_release(tag, f"Woodpecker {base}; backend revision {match[2]}. Source: {sha}. Native macOS ARM64; see the bundled README for requirements.")
    if ref_sha(tag) != sha:
        raise RuntimeError("Revision tag does not match the built commit")
    # Never overwrite an immutable revision's assets. A partial upload can be
    # resumed only when any existing files are byte-for-byte identical.
    for name in ASSETS:
        upload_asset(current, name, dist / name)
    publish_release(current)

    # Re-read all successful numbered releases under the serialized publish job.
    pages = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/releases?per_page=100",
                          "--paginate", "--slurp"))
    candidates = [(int(match[2]), tag)]
    for page in pages:
        for item in page:
            revision = re.fullmatch(re.escape(base) + r"-([1-9]\d*)", item["tag_name"])
            if revision and not item["draft"] and not item["prerelease"] and set(ASSETS) <= {a["name"] for a in item["assets"]}:
                candidates.append((int(revision[1]), item["tag_name"]))
    newest = max(candidates)[1]
    newest_sha = ref_sha(newest)
    newest_release = current if newest == tag else release(newest)
    if newest_release is None:
        raise RuntimeError("Newest successful release is not visible; retry publishing")
    alias = f"{base}-latest"
    with tempfile.TemporaryDirectory() as tmp:
        for name in ASSETS:
            asset = next(a for a in newest_release["assets"] if a["name"] == name)
            Path(tmp, name).write_bytes(download_asset(asset))
        subprocess.run(["shasum", "-a", "256", "-c", "SHA256SUMS"], cwd=tmp, check=True)
        refs = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/git/matching-refs/tags/{alias}"))
        if any(r["ref"] == f"refs/tags/{alias}" for r in refs):
            if ref_sha(alias) != newest_sha:
                # gh -F parses a boolean rather than sending a string.
                gh("api", f"repos/{os.environ['GH_REPO']}/git/refs/tags/{alias}", "--method", "PATCH",
                   "-f", f"sha={newest_sha}", "-F", "force=true")
        else:
            api("git/refs", "POST", ref=f"refs/tags/{alias}", sha=newest_sha)
        notes = f"Rolling alias for Woodpecker {base}. Current revision: {newest}. Source: {newest_sha}. Assets are exact copies of the immutable revision; pin that revision for reproducible deployments."
        alias_release = release(alias)
        if alias_release is None:
            alias_release = create_release(alias, notes)
        for name in ASSETS:
            upload_asset(alias_release, name, Path(tmp, name), replace=True)
        publish_release(alias_release, notes)


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("Publishing is only allowed from GitHub Actions")
    publish(os.environ["RELEASE_TAG"], os.environ["SOURCE_SHA"], Path("dist").resolve())


if __name__ == "__main__":
    main()
