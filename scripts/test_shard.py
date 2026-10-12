#!/usr/bin/env python3
"""Run one deterministic shard of Go tests, examples, and fuzz seeds."""

import argparse
import re
import subprocess
import sys


def select_tests(output, shard, total):
    """Keep a test name in exactly one shard, including duplicate package names."""
    names = sorted({line for line in output.splitlines()
                    if re.fullmatch(r"(?:Test|Example|Fuzz)\S*", line)})
    return names[shard - 1::total]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--shard", type=int, required=True, help="one-based shard index")
    parser.add_argument("--total", type=int, required=True, help="number of shards")
    parser.add_argument("packages", nargs="+", help="Go package patterns")
    args = parser.parse_args(argv)
    if not 1 <= args.shard <= args.total:
        parser.error("require 1 <= shard <= total")

    # Go enumerates the actual runnable tests rather than guessing from source.
    # Subtests stay with their parent; subprocess helper tests remain in the binary.
    listed = subprocess.run(["go", "test", "-list", "^(Test|Example|Fuzz)", *args.packages],
                            capture_output=True, text=True)
    if listed.returncode:
        sys.stdout.write(listed.stdout)
        sys.stderr.write(listed.stderr)
        return listed.returncode
    names = select_tests(listed.stdout, args.shard, args.total)
    if not names:
        print("test shard is empty; check package patterns and shard count", file=sys.stderr)
        return 1

    pattern = "^(" + "|".join(re.escape(name) for name in names) + ")$"
    print(f"Running shard {args.shard}/{args.total}: {len(names)} test names", flush=True)
    # Force execution even when a restored build cache includes test results.
    return subprocess.run(["go", "test", "-json", "-count=1", "-timeout=30m",
                           "-run", pattern, *args.packages]).returncode


if __name__ == "__main__":
    sys.exit(main())
