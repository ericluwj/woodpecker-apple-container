import hashlib
import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("publisher", Path(__file__).with_name("publish-release.py"))
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)


class FakeGitHub:
    def __init__(self):
        self.releases = {}
        self.refs = {}
        self.payloads = {}
        self.calls = []
        self.hide_drafts = False
        self.hide_published = False
        self.asset_failures = 0
        self.release_failures = 0

    def gh(self, *args, binary=False):
        self.calls.append(args)
        assert args[0] == "api", "Publishing must use release IDs, not tag-based gh release commands"
        path = args[1]
        if path.startswith("https://uploads.github.com/"):
            path = path.removeprefix("https://uploads.github.com/")
        path = path.split("/", 3)[3]
        fields = {}
        for i, arg in enumerate(args):
            if arg in ("-f", "-F"):
                key, value = args[i + 1].split("=", 1)
                fields[key] = json.loads(value) if arg == "-F" else value
        method = args[args.index("--method") + 1] if "--method" in args else "GET"
        if path.startswith("releases?"):
            return json.dumps([[r for r in self.releases.values()
                                if not (self.hide_drafts and r["draft"])
                                and not (self.hide_published and not r["draft"])]])
        if path.startswith("git/matching-refs/tags/"):
            prefix = path.removeprefix("git/matching-refs/tags/")
            return json.dumps([{"ref": "refs/tags/" + tag} for tag in self.refs if tag.startswith(prefix)])
        if path.startswith("git/ref/tags/"):
            return json.dumps({"object": {"type": "commit", "sha": self.refs[path.removeprefix("git/ref/tags/")]}})
        if path == "git/refs":
            self.refs[fields["ref"].removeprefix("refs/tags/")] = fields["sha"]
            return "null"
        if path.startswith("git/refs/tags/"):
            self.refs[path.removeprefix("git/refs/tags/")] = fields["sha"]
            return "null"
        if path == "releases" and method == "POST":
            tag = fields["tag_name"]
            assert tag not in self.releases
            assert fields["draft"] is True
            assert fields["prerelease"] is False
            assert "target_commitish" not in fields
            rid = len(self.releases) + 1
            record = dict(fields, id=rid, assets=[],
                          upload_url=f"https://uploads.github.com/repos/test/backend/releases/{rid}/assets{{?name,label}}")
            self.releases[tag] = record
            return json.dumps(record)
        if path.startswith("releases/assets/"):
            aid = int(path.rsplit("/", 1)[1])
            for tag, record in self.releases.items():
                for asset in record["assets"]:
                    if asset["id"] == aid:
                        if method == "DELETE":
                            record["assets"].remove(asset)
                            del self.payloads[tag, asset["name"]]
                            return "null"
                        if self.asset_failures:
                            self.asset_failures -= 1
                            raise publisher.GitHubError(404, "Asset not yet visible")
                        assert binary
                        return self.payloads[tag, asset["name"]]
            raise AssertionError("Unknown asset")
        if path.startswith("releases/"):
            rid = int(path.split("/")[1])
            record = next(r for r in self.releases.values() if r["id"] == rid)
            if method == "GET":
                if self.release_failures:
                    self.release_failures -= 1
                    raise publisher.GitHubError(404, "Release not yet visible")
                return json.dumps(record)
            if method == "PATCH":
                assert fields["draft"] is False and fields["prerelease"] is False
                record.update(fields)
                return json.dumps(record)
            if "/assets?name=" in path:
                name = path.split("?name=")[1]
                assert not any(a["name"] == name for a in record["assets"])
                data = Path(args[args.index("--input") + 1]).read_bytes()
                aid = max([a["id"] for r in self.releases.values() for a in r["assets"]] + [0]) + 1
                asset = {"id": aid, "name": name}
                record["assets"].append(asset)
                self.payloads[record["tag_name"], name] = data
                return json.dumps(asset)
        raise AssertionError(args)


class ReleasePolicyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dist = Path(self.tmp.name)
        self.fake = FakeGitHub()
        env = patch.dict(os.environ, GH_REPO="test/backend")
        env.start()
        self.addCleanup(env.stop)
        self.real_gh = publisher.gh
        mock = patch.object(publisher, "gh", self.fake.gh)
        mock.start()
        self.addCleanup(mock.stop)
        self.package(b"first build")

    def package(self, data):
        (self.dist / publisher.ASSETS[0]).write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()
        (self.dist / "SHA256SUMS").write_text(f"{digest}  {publisher.ASSETS[0]}\n")

    def test_versions_and_out_of_order_completion(self):
        publisher.publish("3.19.0-1", "a" * 40, self.dist)
        publisher.publish("3.20.0-2", "b" * 40, self.dist)
        self.package(b"older revision finishing late")
        publisher.publish("3.20.0-1", "c" * 40, self.dist)
        self.assertEqual(self.fake.refs["3.20.0-latest"], "b" * 40)
        self.assertEqual(self.fake.refs["3.19.0-latest"], "a" * 40)
        for asset in publisher.ASSETS:
            self.assertEqual(self.fake.payloads["3.20.0-latest", asset], self.fake.payloads["3.20.0-2", asset])
        self.assertFalse(self.fake.releases["3.20.0-latest"]["prerelease"])

    def test_identical_retry_and_immutable_content(self):
        publisher.publish("3.20.0-1", "a" * 40, self.dist)
        publisher.publish("3.20.0-1", "a" * 40, self.dist)
        self.package(b"changed binary")
        with self.assertRaisesRegex(RuntimeError, "immutable asset"):
            publisher.publish("3.20.0-1", "a" * 40, self.dist)
        self.assertEqual(self.fake.payloads["3.20.0-1", publisher.ASSETS[0]], b"first build")

    def test_tag_conflict(self):
        self.fake.refs["3.20.0-1"] = "b" * 40
        with self.assertRaisesRegex(RuntimeError, "different commit"):
            publisher.publish("3.20.0-1", "a" * 40, self.dist)
        self.assertFalse(self.fake.releases)

    def test_partial_alias_upload_can_resume(self):
        original = self.fake.gh
        failed = False

        def interrupted(*args, **kwargs):
            nonlocal failed
            if args[1].startswith("https://uploads.github.com/") and "3.20.0-latest" in self.fake.releases and not failed:
                failed = True
                raise RuntimeError("upload interrupted")
            return original(*args, **kwargs)

        with patch.object(publisher, "gh", interrupted):
            with self.assertRaisesRegex(RuntimeError, "interrupted"):
                publisher.publish("3.20.0-1", "a" * 40, self.dist)
        publisher.publish("3.20.0-1", "a" * 40, self.dist)
        for asset in publisher.ASSETS:
            self.assertEqual(self.fake.payloads["3.20.0-latest", asset], self.fake.payloads["3.20.0-1", asset])

    def test_revision_order_is_numeric(self):
        publisher.publish("3.20.0-10", "a" * 40, self.dist)
        publisher.publish("3.20.0-2", "b" * 40, self.dist)
        self.assertEqual(self.fake.refs["3.20.0-latest"], "a" * 40)

    def test_new_drafts_are_not_visible_in_release_list(self):
        # Reproduce the production failure: POST succeeds, list omits the draft.
        self.fake.hide_drafts = True
        self.fake.hide_published = True
        publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.assertFalse(self.fake.releases["3.20.1-1"]["draft"])
        self.assertFalse(self.fake.releases["3.20.1-latest"]["draft"])
        for asset in publisher.ASSETS:
            self.assertEqual(self.fake.payloads["3.20.1-1", asset], self.fake.payloads["3.20.1-latest", asset])

    def test_noop_alias_does_not_require_tag_write(self):
        publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.fake.calls.clear()
        publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.assertFalse(any("/git/refs/tags/" in c[1] for c in self.fake.calls))

    def test_temporary_asset_visibility_is_retried(self):
        self.fake.asset_failures = 2
        with patch.object(publisher.time, "sleep") as sleep:
            publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.assertEqual(sleep.call_count, 2)

    def test_created_release_visibility_is_retried_by_id(self):
        self.fake.hide_drafts = True
        self.fake.release_failures = 2
        with patch.object(publisher.time, "sleep") as sleep:
            publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.assertEqual(sleep.call_count, 2)
        self.assertFalse(self.fake.releases["3.20.1-latest"]["draft"])

    def test_alias_permission_failure_preserves_existing_alias(self):
        publisher.publish("3.20.1-1", "a" * 40, self.dist)
        original = self.fake.gh
        def forbidden(*args, **kwargs):
            if "/git/refs/tags/" in args[1]:
                raise publisher.GitHubError(403, "Tag update denied")
            return original(*args, **kwargs)
        self.package(b"second build")
        with patch.object(publisher, "gh", forbidden):
            with self.assertRaisesRegex(publisher.GitHubError, "Tag update denied"):
                publisher.publish("3.20.1-2", "b" * 40, self.dist)
        self.assertFalse(self.fake.releases["3.20.1-2"]["draft"])
        self.assertEqual(self.fake.refs["3.20.1-latest"], "a" * 40)
        self.assertEqual(self.fake.payloads["3.20.1-latest", publisher.ASSETS[0]], b"first build")

    def test_visibility_retry_is_bounded(self):
        self.fake.asset_failures = 10
        with patch.object(publisher.time, "sleep") as sleep:
            with self.assertRaises(publisher.GitHubError):
                publisher.publish("3.20.1-1", "a" * 40, self.dist)
        self.assertEqual(sleep.call_count, 4)
        self.assertNotIn("3.20.1-latest", self.fake.refs)

    def test_permission_failure_is_not_retried(self):
        with patch.object(publisher, "gh", side_effect=publisher.GitHubError(403, "Permission denied")):
            with patch.object(publisher.time, "sleep") as sleep:
                with self.assertRaises(publisher.GitHubError):
                    publisher.publish("3.20.1-1", "a" * 40, self.dist)
        sleep.assert_not_called()
        self.assertFalse(self.fake.refs)

    def test_cli_permission_error_explains_required_access(self):
        error = subprocess.CalledProcessError(1, ["gh"], stderr=b"gh: Resource not accessible by integration (HTTP 403)")
        # Exercise the real adapter rather than the fake API implementation.
        with patch.object(publisher, "gh", self.real_gh):
            with patch.object(publisher.subprocess, "check_output", side_effect=error):
                with self.assertRaisesRegex(publisher.GitHubError, "contents:write"):
                    publisher.gh("api", "repos/test/backend/git/refs")

    def test_entrypoint_rejects_local_publishing(self):
        with patch.dict(os.environ, GITHUB_ACTIONS="false"):
            with self.assertRaisesRegex(RuntimeError, "only allowed from GitHub Actions"):
                publisher.main()
        self.assertFalse(self.fake.calls)

    def test_entrypoint_allows_actions(self):
        with patch.dict(os.environ, GITHUB_ACTIONS="true", RELEASE_TAG="3.20.1-1", SOURCE_SHA="a" * 40):
            with patch.object(publisher, "publish") as publish:
                publisher.main()
        publish.assert_called_once_with("3.20.1-1", "a" * 40, Path("dist").resolve())

    def test_invalid_revision(self):
        for tag in ("3.20.0", "3.20.0-latest", "3.20.0-0", "3.20.0-01", "3.20.0-wp.1"):
            with self.assertRaises(ValueError):
                publisher.publish(tag, "a" * 40, self.dist)
        self.assertFalse(self.fake.refs)


if __name__ == "__main__":
    unittest.main()
