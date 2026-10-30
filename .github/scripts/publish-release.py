"""Publish immutable revisions and refresh the matching rolling GitHub Release."""
import json
import os
import re
import subprocess
import tempfile
from pathlib import Path

ASSETS = ("woodpecker-agent-apple-darwin-arm64.tar.gz", "SHA256SUMS")


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True).strip()


def api(path, method="GET", **fields):
    args = ["api", f"repos/{os.environ['GH_REPO']}/{path}", "--method", method]
    for key, value in fields.items():
        args += ["-f", f"{key}={value}"]
    return json.loads(gh(*args) or "null")


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
        gh("release", "create", tag, "--target", sha, "--title", tag, "--draft",
           "--notes", f"Woodpecker {base}; backend revision {match[2]}. Source: {sha}. Native macOS ARM64; see the bundled README for requirements.")
        current = release(tag)
    if ref_sha(tag) != sha:
        raise RuntimeError("Revision tag does not match the built commit")
    # Never overwrite an immutable revision's assets. A partial upload can be
    # resumed only when any existing files are byte-for-byte identical.
    existing = {a["name"] for a in current["assets"]}
    with tempfile.TemporaryDirectory() as tmp:
        for name in ASSETS:
            if name in existing:
                gh("release", "download", tag, "--pattern", name, "--dir", tmp)
                if Path(tmp, name).read_bytes() != (dist / name).read_bytes():
                    raise RuntimeError(f"Refusing to replace immutable asset {tag}/{name}")
            else:
                gh("release", "upload", tag, str(dist / name))
    gh("release", "edit", tag, "--draft=false", "--prerelease=false", "--latest=false")

    # Re-read all successful numbered releases under the serialized publish job.
    pages = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/releases?per_page=100",
                          "--paginate", "--slurp"))
    candidates = []
    for page in pages:
        for item in page:
            revision = re.fullmatch(re.escape(base) + r"-([1-9]\d*)", item["tag_name"])
            if revision and not item["draft"] and not item["prerelease"] and set(ASSETS) <= {a["name"] for a in item["assets"]}:
                candidates.append((int(revision[1]), item["tag_name"]))
    newest = max(candidates)[1]
    newest_sha = ref_sha(newest)
    alias = f"{base}-latest"
    with tempfile.TemporaryDirectory() as tmp:
        for name in ASSETS:
            gh("release", "download", newest, "--pattern", name, "--dir", tmp)
        subprocess.run(["shasum", "-a", "256", "-c", "SHA256SUMS"], cwd=tmp, check=True)
        refs = json.loads(gh("api", f"repos/{os.environ['GH_REPO']}/git/matching-refs/tags/{alias}"))
        if any(r["ref"] == f"refs/tags/{alias}" for r in refs):
            # gh -F parses a boolean rather than sending a string.
            gh("api", f"repos/{os.environ['GH_REPO']}/git/refs/tags/{alias}", "--method", "PATCH",
               "-f", f"sha={newest_sha}", "-F", "force=true")
        else:
            api("git/refs", "POST", ref=f"refs/tags/{alias}", sha=newest_sha)
        notes = f"Rolling alias for Woodpecker {base}. Current revision: {newest}. Source: {newest_sha}. Assets are exact copies of the immutable revision; pin that revision for reproducible deployments."
        if release(alias) is None:
            gh("release", "create", alias, "--verify-tag", "--title", alias, "--notes", notes, "--latest=false")
        for name in ASSETS:
            gh("release", "upload", alias, str(Path(tmp, name)), "--clobber")
        gh("release", "edit", alias, "--notes", notes, "--prerelease=false", "--latest=false")


if __name__ == "__main__":
    publish(os.environ["RELEASE_TAG"], os.environ["SOURCE_SHA"], Path("dist").resolve())
