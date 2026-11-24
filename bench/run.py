#!/usr/bin/env python3
"""Harness benchmark runner: tasks x configurations x repeats, through Pitwall.

Each run copies a task from bench/tasks to a scratch directory and runs
Pitwall's own agent (bin/agent) on it through a running gateway. The gateway
groups the run's calls into one task (x-pitwall-task), prices them and
records tokens, cache use and guard events; the runner then:

1. checks the result itself: it puts back the task's original test files
   (an agent that edits the tests to pass gets no credit) and runs
   `go test ./...`;
2. reports that verified outcome to the gateway, so dashboards agree;
3. reads the task's cost and token totals from the admin API;
4. appends one row to <out>/runs.csv and saves the agent's diff.

A plan file (JSON) lists the configurations. Each gets its own virtual key,
so the cache optimizer can be on for one and off for another. All keys
belong to one new team whose monthly budget is the plan's budget_usd: when
the money runs out the gateway refuses calls with 402 and the runner stops.
Runs already in runs.csv are skipped, so an interrupted benchmark resumes
where it stopped (with new keys under the same team and budget).

For the mock provider, a config can set "mock": {"solve_rate", "loop_rate",
"cheat_rate"}. The runner then scripts the mock (see [[mock:script=...]]) to
replay the reference fix from bench/solutions, to loop, to "pass" by
replacing the tests with an empty one, or otherwise to look around and give
up. That exercises every part of the pipeline; it does not measure a model.

Only the standard library is used.

    python3 bench/run.py --plan bench/plans/dry-run.json --out bench/results/dry-run
"""

import argparse
import base64
import concurrent.futures
import csv
import difflib
import fnmatch
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TASKS = os.path.join(ROOT, "bench", "tasks")
SOLUTIONS = os.path.join(ROOT, "bench", "solutions")

COLUMNS = [
    "plan", "config", "model", "cache_optimizer", "keep_tool_results", "trim_batch", "task", "repeat",
    "label", "task_id", "outcome", "agent_outcome", "tests_modified", "turns", "calls", "failed_calls",
    "stop_reason", "error", "cost_usd", "prompt_tokens", "input_tokens", "cached_read_tokens",
    "cache_write_tokens", "output_tokens", "repeated_input_tokens", "max_context_tokens",
    "agent_seconds", "gateway_seconds", "guard_events", "guard_stopped", "inferred_task_ids",
    "mock_behavior", "started_at",
]


class Stop(Exception):
    """Ends the whole benchmark (budget exhausted, gateway down)."""


def api(base, method, path, token, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base.rstrip("/") + path, data=data, method=method,
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def load_plan(path):
    with open(path) as f:
        plan = json.load(f)
    plan.setdefault("repeats", 1)
    plan.setdefault("max_turns", 30)
    plan.setdefault("max_tokens", 8192)
    plan.setdefault("budget_usd", 0)
    plan.setdefault("tasks", ["*"])
    labels = [c["label"] for c in plan["configs"]]
    if len(set(labels)) != len(labels):
        sys.exit("config labels must be unique")
    for c in plan["configs"]:
        c.setdefault("cache_optimizer", False)
        c.setdefault("keep_tool_results", 0)
        c.setdefault("trim_batch", 1)
        if not c["model"].startswith("mock-") and not plan["budget_usd"] > 0:
            sys.exit(f"config {c['label']} uses a paid model; set budget_usd above 0 in the plan")
    return plan


def pick_tasks(patterns):
    names = sorted(d for d in os.listdir(TASKS) if os.path.isfile(os.path.join(TASKS, d, "TASK.md")))
    return [n for n in names if any(fnmatch.fnmatch(n, p) for p in patterns)]


def is_test(name):
    return name.endswith("_test.go")


def source_files(task_dir):
    out = []
    for dirpath, _, files in os.walk(task_dir):
        for f in files:
            rel = os.path.relpath(os.path.join(dirpath, f), task_dir)
            out.append(rel)
    return sorted(out)


# --- mock scripting -----------------------------------------------------------

def mock_behavior(cfg, task, repeat):
    m = cfg.get("mock")
    if not m:
        return ""
    # The draw depends on the task and repeat, not the config, so configs with
    # the same rates get the same behavior on each run and compare paired.
    u = int(hashlib.sha256(f"{m.get('seed', '')}|{task}|{repeat}".encode()).hexdigest()[:8], 16) / 2**32
    loop, solve, cheat = m.get("loop_rate", 0), m.get("solve_rate", 0), m.get("cheat_rate", 0)
    if u < loop:
        return "loop"
    if u < loop + solve:
        return "solve"
    if u < loop + solve + cheat:
        return "cheat"
    return "flail"


def mock_directive(behavior, task):
    if behavior == "loop":
        return "[[mock:mode=loop]]"
    task_dir = os.path.join(TASKS, task)
    code = [f for f in source_files(task_dir) if f.endswith(".go") and not is_test(f)]
    steps = [{"name": "list_files", "input": {}}, {"name": "run_tests"}]
    steps += [{"name": "read_file", "input": {"path": f}} for f in code]
    if behavior == "solve":  # read the tests, try a change that doesn't help, then the reference fix
        steps += [{"name": "read_file", "input": {"path": f}} for f in source_files(task_dir) if is_test(f)]
        with open(os.path.join(task_dir, code[0])) as fh:
            steps.append({"name": "write_file", "input": {"path": code[0], "content": fh.read() + "\n"}})
        steps.append({"name": "run_tests"})
        sol = os.path.join(SOLUTIONS, task)
        for f in sorted(os.listdir(sol)):
            with open(os.path.join(sol, f)) as fh:
                steps.append({"name": "write_file", "input": {"path": f, "content": fh.read()}})
        steps.append({"name": "run_tests"})
    elif behavior == "cheat":  # replace every test file with one empty test
        for f in source_files(task_dir):
            if is_test(f):
                with open(os.path.join(task_dir, f)) as fh:
                    pkg = next(l.split()[1] for l in fh if l.startswith("package "))
                steps.append({"name": "write_file", "input": {"path": f, "content":
                              f'package {pkg}\n\nimport "testing"\n\nfunc TestOK(t *testing.T) {{}}\n'}})
        steps.append({"name": "run_tests"})
    else:  # flail: look around, rerun the tests, give up
        steps += [{"name": "read_file", "input": {"path": f}} for f in source_files(task_dir) if is_test(f)]
        steps.append({"name": "run_tests"})
    enc = base64.urlsafe_b64encode(json.dumps(steps).encode()).decode().rstrip("=")
    return f"[[mock:script={enc}]]"


# --- one run ------------------------------------------------------------------

def verify(task_dir, work):
    """Restores the original tests and runs them. Returns (passed, tests_modified)."""
    modified = False
    original = {f for f in source_files(task_dir) if is_test(f)}
    for f in source_files(work):
        if is_test(f) and f not in original:
            os.remove(os.path.join(work, f))
            modified = True
    for f in original:
        src, dst = os.path.join(task_dir, f), os.path.join(work, f)
        with open(src, "rb") as a:
            want = a.read()
        got = None
        if os.path.exists(dst):
            with open(dst, "rb") as b:
                got = b.read()
        if got != want:
            modified = True
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            with open(dst, "wb") as b:
                b.write(want)
    try:
        r = subprocess.run(["go", "test", "-count=1", "./..."], cwd=work, capture_output=True, timeout=180)
        return r.returncode == 0, modified
    except subprocess.TimeoutExpired:
        return False, modified


def diff(task_dir, work):
    out = []
    names = sorted(set(source_files(task_dir)) | set(source_files(work)))
    for f in names:
        if is_test(f):
            continue
        a = open(os.path.join(task_dir, f)).read().splitlines(keepends=True) if os.path.exists(os.path.join(task_dir, f)) else []
        b = open(os.path.join(work, f)).read().splitlines(keepends=True) if os.path.exists(os.path.join(work, f)) else []
        out.extend(difflib.unified_diff(a, b, "a/" + f, "b/" + f))
    return "".join(out)


def task_detail(gateway, admin, task_id, expect_calls):
    # Calls are written in batches; wait until all of the run's calls are in.
    d = None
    for _ in range(40):
        d = api(gateway, "GET", f"/admin/tasks/{task_id}", admin)
        if d["task"]["calls"] >= expect_calls:
            break
        time.sleep(0.1)
    return d


def run_one(args, plan, cfg, key, task, repeat):
    task_dir = os.path.join(TASKS, task)
    with open(os.path.join(task_dir, "TASK.md")) as f:
        prompt = f.read()
    behavior = mock_behavior(cfg, task, repeat)
    if behavior:
        prompt += "\n" + mock_directive(behavior, task) + "\n"
    stamp = datetime.now(timezone.utc)
    label = f"bench/{plan['name']}/{cfg['label']}/{task}/r{repeat}/{stamp.strftime('%Y%m%dT%H%M%S%f')}"
    row = {c: "" for c in COLUMNS}
    row.update(plan=plan["name"], config=cfg["label"], model=cfg["model"], cache_optimizer=int(bool(cfg["cache_optimizer"])),
               keep_tool_results=cfg["keep_tool_results"], trim_batch=cfg["trim_batch"], task=task, repeat=repeat, label=label,
               mock_behavior=behavior, started_at=stamp.isoformat(timespec="seconds"))

    with tempfile.TemporaryDirectory(prefix="pitwall-bench-") as tmp:
        work = os.path.join(tmp, task)
        shutil.copytree(task_dir, work)
        cmd = [args.agent, "-gateway", plan["gateway"], "-model", cfg["model"], "-dir", work, "-prompt", prompt,
               "-task", label, "-config", cfg["label"], "-max-turns", str(plan["max_turns"]),
               "-max-tokens", str(plan["max_tokens"]), "-keep-tool-results", str(cfg["keep_tool_results"]),
               "-trim-batch", str(cfg["trim_batch"])]
        try:
            p = subprocess.run(cmd, env={**os.environ, "PITWALL_KEY": key}, capture_output=True, text=True,
                               timeout=args.run_timeout)
            res = json.loads(p.stdout) if p.stdout.strip() else {"outcome": "failed", "error": p.stderr.strip()[-300:]}
        except subprocess.TimeoutExpired:
            res = {"outcome": "failed", "error": f"agent timed out after {args.run_timeout}s"}
        err = res.get("error", "")
        if ": 402 " in err:  # the SDK's error text: POST "<url>": 402 Payment Required
            raise Stop(f"the gateway refused a call for budget: {err[:200]}")
        passed, modified = verify(task_dir, work)
        patch = diff(task_dir, work)

    outcome = "solved" if passed else ("stopped" if res.get("outcome") == "stopped" else "failed")
    row.update(task_id=res.get("task_id", ""), outcome=outcome, agent_outcome=res.get("outcome", ""),
               tests_modified=int(modified), turns=res.get("turns", 0), stop_reason=res.get("stop_reason", ""),
               error=" ".join(err.split())[:300], agent_seconds=round(res.get("seconds", 0), 2))
    if row["task_id"]:
        if outcome != res.get("outcome"):
            try:
                api(plan["gateway"], "POST", "/v1/pitwall/tasks/outcome", key, {"task": label, "outcome": outcome})
            except urllib.error.URLError as e:
                print(f"  could not report the verified outcome: {e}", file=sys.stderr)
        d = task_detail(plan["gateway"], args.admin_token, row["task_id"], row["turns"])
        t = d["task"]
        for k in ("calls", "failed_calls", "cost_usd", "prompt_tokens", "input_tokens", "cached_read_tokens",
                  "cache_write_tokens", "output_tokens", "repeated_input_tokens", "max_context_tokens", "guard_events"):
            row[k] = t.get(k, 0)
        row["gateway_seconds"] = round(t.get("wall_seconds", 0), 2)
        calls = d.get("calls") or []
        row["guard_stopped"] = int(any(c.get("guard_action") == "stop" for c in calls))
        row["inferred_task_ids"] = ";".join(sorted({c["inferred_task_id"] for c in calls if c.get("inferred_task_id")}))
    if patch:
        pdir = os.path.join(args.out, "diffs", cfg["label"])
        os.makedirs(pdir, exist_ok=True)
        with open(os.path.join(pdir, f"{task}-r{repeat}.diff"), "w") as f:
            f.write(patch)
    return row


# --- setup and main loop --------------------------------------------------------

def setup(args, plan, state):
    """Creates the team (once per benchmark) and a fresh key per config."""
    gw, admin = plan["gateway"], args.admin_token
    if "team_id" not in state:
        team = api(gw, "POST", "/admin/teams", admin,
                   {"name": f"bench {plan['name']} {datetime.now(timezone.utc):%Y-%m-%d %H:%M:%S}",
                    "monthly_budget_usd": plan["budget_usd"]})
        user = api(gw, "POST", "/admin/users", admin,
                   {"email": f"bench+{team['id']}@pitwall.local", "team_id": team["id"], "role": "dev"})
        state.update(team_id=team["id"], user_id=user["id"])
    keys, ids = {}, []
    for cfg in plan["configs"]:
        k = api(gw, "POST", "/admin/keys", admin,
                {"user_id": state["user_id"], "name": f"bench {plan['name']} {cfg['label']}",
                 "allowed_models": [cfg["model"]], "cache_optimizer_on": bool(cfg["cache_optimizer"])})
        keys[cfg["label"]] = k["key"]
        ids.append(k["id"])
    state.setdefault("key_ids", []).extend(ids)
    return keys, ids


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--plan", required=True)
    p.add_argument("--out", required=True, help="results directory (runs.csv, run.json, diffs/)")
    p.add_argument("--admin-token", default=os.environ.get("PITWALL_ADMIN_TOKEN", ""))
    p.add_argument("--agent", default=os.path.join(ROOT, "bin", "agent"))
    p.add_argument("--gateway", help="override the plan's gateway URL")
    p.add_argument("--jobs", type=int, default=1, help="runs in parallel")
    p.add_argument("--run-timeout", type=int, default=1200, help="seconds before an agent run is killed")
    p.add_argument("--limit", type=int, default=0, help="stop after this many new runs (0: no limit)")
    args = p.parse_args()
    if not args.admin_token:
        sys.exit("give --admin-token or set PITWALL_ADMIN_TOKEN")
    plan = load_plan(args.plan)
    if args.gateway:
        plan["gateway"] = args.gateway
    tasks = pick_tasks(plan["tasks"])
    if not tasks:
        sys.exit("no tasks match the plan")
    os.makedirs(args.out, exist_ok=True)

    state_path = os.path.join(args.out, "run.json")
    state = {}
    if os.path.exists(state_path):
        with open(state_path) as f:
            state = json.load(f)
        if state.get("plan", {}).get("name") != plan["name"]:
            sys.exit(f"{args.out} holds results of another plan")
    runs_path = os.path.join(args.out, "runs.csv")
    done = set()
    if os.path.exists(runs_path):
        with open(runs_path) as f:
            done = {(r["config"], r["task"], int(r["repeat"])) for r in csv.DictReader(f)}

    todo = [(cfg, t, r) for r in range(1, plan["repeats"] + 1) for t in tasks for cfg in plan["configs"]
            if (cfg["label"], t, r) not in done]
    if args.limit:
        todo = todo[:args.limit]
    print(f"{plan['name']}: {len(tasks)} tasks x {len(plan['configs'])} configs x {plan['repeats']} repeats; "
          f"{len(done)} done, {len(todo)} to run")
    if not todo:
        return

    keys, key_ids = setup(args, plan, state)
    state["plan"] = plan
    state.setdefault("started_at", datetime.now(timezone.utc).isoformat(timespec="seconds"))
    with open(state_path, "w") as f:
        json.dump(state, f, indent=2)

    new_file = not os.path.exists(runs_path)
    lock, stop = threading.Lock(), threading.Event()
    out = open(runs_path, "a", newline="")
    writer = csv.DictWriter(out, fieldnames=COLUMNS)
    if new_file:
        writer.writeheader()
    count, failures = [0], [0]

    def work(item):
        cfg, task, repeat = item
        if stop.is_set():
            return
        try:
            row = run_one(args, plan, cfg, keys[cfg["label"]], task, repeat)
        except Stop as e:
            stop.set()
            print(f"stopping: {e}", file=sys.stderr)
            return
        except (urllib.error.URLError, OSError, KeyError, ValueError) as e:
            print(f"  {cfg['label']} {task} r{repeat}: run failed: {e}", file=sys.stderr)
            with lock:
                failures[0] += 1
            return
        with lock:
            writer.writerow(row)
            out.flush()
            count[0] += 1
            print(f"[{count[0]}/{len(todo)}] {cfg['label']:<24} {task:<22} r{repeat} {row['outcome']:<8} "
                  f"turns={row['turns']:<3} ${float(row['cost_usd'] or 0):.4f}")

    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
            list(pool.map(work, todo))
    except KeyboardInterrupt:
        stop.set()
        print("interrupted; finished runs are saved", file=sys.stderr)
    finally:
        out.close()
        for kid in key_ids:  # the keys were for this invocation only
            try:
                api(plan["gateway"], "DELETE", f"/admin/keys/{kid}", args.admin_token)
            except urllib.error.URLError:
                pass
    try:
        team = next(t for t in api(plan["gateway"], "GET", "/admin/teams", args.admin_token)["data"]
                    if t["id"] == state["team_id"])
        print(f"team spend so far: ${team.get('spent_this_month_usd', 0):.4f} of ${plan['budget_usd']}")
    except (urllib.error.URLError, StopIteration, KeyError):
        pass
    if failures[0]:
        sys.exit(f"{failures[0]} runs could not be recorded; run again to retry them")


if __name__ == "__main__":
    main()
