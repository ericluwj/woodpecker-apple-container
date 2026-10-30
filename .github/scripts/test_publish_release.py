import hashlib
import importlib.util
import json
import os
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

    def gh(self, *args):
        if args[0] == "api":
            path = args[1].split("/", 3)[3]
            if path.startswith("releases?"):
                return json.dumps([list(self.releases.values())])
            fields = dict(args[i + 1].split("=", 1) for i, arg in enumerate(args) if arg in ("-f", "-F"))
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
            raise AssertionError(args)
        _, action, tag, *opts = args
        def option(key):
            return opts[opts.index(key) + 1]
        if action == "create":
            self.releases[tag] = {"tag_name": tag, "draft": "--draft" in opts, "prerelease": False, "assets": []}
        elif action == "upload":
            file = Path(opts[0])
            name = file.name
            if (tag, name) in self.payloads and "--clobber" not in opts:
                raise AssertionError("Immutable asset overwritten")
            self.payloads[tag, name] = file.read_bytes()
            self.releases[tag]["assets"] = [{"name": n} for t, n in self.payloads if t == tag]
        elif action == "download":
            name = option("--pattern")
            Path(option("--dir"), name).write_bytes(self.payloads[tag, name])
        elif action == "edit":
            self.releases[tag]["draft"] = False
        else:
            raise AssertionError(args)
        return ""


class ReleasePolicyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dist = Path(self.tmp.name)
        self.fake = FakeGitHub()
        env = patch.dict(os.environ, GH_REPO="test/backend")
        env.start()
        self.addCleanup(env.stop)
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

        def interrupted(*args):
            nonlocal failed
            if args[:3] == ("release", "upload", "3.20.0-latest") and not failed:
                failed = True
                raise RuntimeError("upload interrupted")
            return original(*args)

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

    def test_invalid_revision(self):
        for tag in ("3.20.0", "3.20.0-latest", "3.20.0-0", "3.20.0-01", "3.20.0-wp.1"):
            with self.assertRaises(ValueError):
                publisher.publish(tag, "a" * 40, self.dist)
        self.assertFalse(self.fake.refs)


if __name__ == "__main__":
    unittest.main()
