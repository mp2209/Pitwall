#!/usr/bin/env python3
"""Checks the benchmark tasks: each must fail as shipped and pass once its
reference fix from bench/solutions is applied, with go vet clean both ways.

    python3 bench/check_tasks.py            # all tasks
    python3 bench/check_tasks.py 007 013    # tasks whose names start with these
"""

import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TASKS = os.path.join(ROOT, "bench", "tasks")
SOLUTIONS = os.path.join(ROOT, "bench", "solutions")


def go(args, cwd):
    return subprocess.run(["go", *args], cwd=cwd, capture_output=True, text=True, timeout=300)


def check(name):
    problems = []
    task, sol = os.path.join(TASKS, name), os.path.join(SOLUTIONS, name)
    if not os.path.isfile(os.path.join(task, "TASK.md")):
        problems.append("no TASK.md")
    if not os.path.isdir(sol) or not os.listdir(sol):
        return problems + ["no reference fix in bench/solutions"]
    with tempfile.TemporaryDirectory() as tmp:
        work = os.path.join(tmp, name)
        shutil.copytree(task, work)
        if go(["vet", "./..."], work).returncode != 0:
            problems.append("go vet fails as shipped")
        if go(["test", "-count=1", "./..."], work).returncode == 0:
            problems.append("tests pass as shipped")
        for f in os.listdir(sol):
            if f.endswith("_test.go"):
                problems.append(f"the fix changes a test file: {f}")
            shutil.copy(os.path.join(sol, f), os.path.join(work, f))
        if go(["vet", "./..."], work).returncode != 0:
            problems.append("go vet fails with the fix")
        r = go(["test", "-count=3", "./..."], work)
        if r.returncode != 0:
            problems.append("tests fail with the fix:\n" + r.stdout[-1500:])
    return problems


def main():
    names = sorted(d for d in os.listdir(TASKS) if os.path.isdir(os.path.join(TASKS, d)))
    if len(sys.argv) > 1:
        names = [n for n in names if any(n.startswith(p) for p in sys.argv[1:])]
    bad = 0
    for n in names:
        problems = check(n)
        print(f"{'ok  ' if not problems else 'FAIL'} {n}")
        for p in problems:
            print("     " + p)
        bad += bool(problems)
    print(f"{len(names) - bad}/{len(names)} tasks ok")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
