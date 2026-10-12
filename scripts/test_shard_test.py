"""Exercise sharding through the CLI with an isolated Go test driver."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from test_shard import select_tests


SCRIPT = Path(__file__).with_name("test_shard.py")


class TestShards(unittest.TestCase):
    def setUp(self):
        fixture = tempfile.TemporaryDirectory(prefix="mergeyard-shard-test-")
        self.addCleanup(fixture.cleanup)
        self.root = Path(fixture.name)
        self.result = self.root / "result.json"
        self.tests = ["TestAlpha", "TestAlphabet", "TestBeta", "TestAlpha",
                      "ExampleValue", "FuzzBytes", "TestΔ"]
        self.env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ["PATH"],
                        SHARD_TEST_NAMES=json.dumps(self.tests), SHARD_TEST_RESULT=str(self.result),
                        PYTHONDONTWRITEBYTECODE="1")
        go = self.root / "go"
        go.write_text("#!" + sys.executable + "\n" + '''
import json, os, pathlib, re, sys
names = json.loads(os.environ["SHARD_TEST_NAMES"])
if "-list" in sys.argv:
    print("\\n".join(names))
    print("ok  example.test/package 0.001s")
    print("?   example.test/empty [no test files]")
    print("listing diagnostic", file=sys.stderr)
    sys.exit(int(os.environ.get("SHARD_TEST_LIST_EXIT", "0")))
pattern = sys.argv[sys.argv.index("-run") + 1]
selected = [name for name in names if re.search(pattern, name)]
pathlib.Path(os.environ["SHARD_TEST_RESULT"]).write_text(json.dumps(selected))
sys.exit(int(os.environ.get("SHARD_TEST_RUN_EXIT", "0")))
''')
        go.chmod(0o700)

    def run_shard(self, shard, total):
        self.result.unlink(missing_ok=True)
        return subprocess.run([sys.executable, str(SCRIPT), "--shard", str(shard),
                               "--total", str(total), "./..."], env=self.env,
                              capture_output=True, text=True)

    def test_every_test_example_and_fuzz_seed_executes_exactly_once(self):
        executed = []
        for shard in range(1, 5):
            result = self.run_shard(shard, 4)
            self.assertEqual(result.returncode, 0, result.stderr)
            executed.extend(json.loads(self.result.read_text()))
        # Includes duplicate names from different packages and similar prefixes.
        self.assertCountEqual(executed, self.tests)

    def test_order_of_package_listings_does_not_change_assignment(self):
        expected = select_tests("\n".join(self.tests), 2, 4)
        self.assertEqual(select_tests("\n".join(reversed(self.tests)), 2, 4), expected)

    def test_test_failure_fails_the_shard(self):
        self.env["SHARD_TEST_RUN_EXIT"] = "1"
        self.assertEqual(self.run_shard(1, 4).returncode, 1)
        self.assertTrue(self.result.exists())

    def test_discovery_failure_is_visible_and_prevents_test_execution(self):
        self.env["SHARD_TEST_LIST_EXIT"] = "1"
        result = self.run_shard(1, 4)
        self.assertEqual(result.returncode, 1)
        self.assertIn("listing diagnostic", result.stderr)
        self.assertFalse(self.result.exists())

    def test_empty_shard_fails_instead_of_silently_skipping_tests(self):
        result = self.run_shard(10, 10)
        self.assertEqual(result.returncode, 1)
        self.assertIn("test shard is empty", result.stderr)
        self.assertFalse(self.result.exists())

    def test_no_matching_tests_fails(self):
        self.env["SHARD_TEST_NAMES"] = "[]"
        self.assertEqual(self.run_shard(1, 4).returncode, 1)
        self.assertFalse(self.result.exists())

    def test_invalid_shard_arguments_fail_before_discovery(self):
        for shard, total in [(0, 4), (5, 4), (1, 0), (-1, -2)]:
            with self.subTest(shard=shard, total=total):
                result = self.run_shard(shard, total)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("require 1 <= shard <= total", result.stderr)
                self.assertFalse(self.result.exists())


if __name__ == "__main__":
    unittest.main()
