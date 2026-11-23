#!/usr/bin/env python3
"""Phase 4 measurements: what the cache optimizer saves, and how well the
runaway-agent guard catches loops without stopping normal runs.

It drives Pitwall's own agent (bin/agent) through a running gateway, on a
copy of a benchmark task, against the mock provider:

- Cache optimizer: the same 12-turn task with the optimizer off and on (two
  keys), with full history and with old tool output trimmed. The mock
  simulates Anthropic's prompt cache (breakpoints, 20-position lookback,
  1,024-token minimum) and the gateway prices the calls.
- Guard: seeded loops, where the mock makes the agent run the same failing
  test command every turn, against the guarded gateway (and, optionally, a
  second gateway with the guard off, for the cost of an unguarded loop). The
  cache runs are normal runs, so they measure false stops.

Only the standard library is used.

    python3 bench/experiments/phase4.py --gateway http://localhost:8180 \\
        --admin-token "$PITWALL_ADMIN_TOKEN" --agent bin/agent \\
        --unguarded-gateway http://localhost:8181 --runs 5 --out bench/results
"""

import argparse
import json
import os
import shutil
import statistics
import subprocess
import tempfile
import time
import urllib.request
from datetime import datetime, timezone

TASK = "bench/tasks/001-word-count"


def api(base, method, path, token, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)


def run_agent(args, gateway, key, directive, config, keep=0, max_turns=30):
    with tempfile.TemporaryDirectory() as tmp:
        work = os.path.join(tmp, "task")
        shutil.copytree(TASK, work)
        prompt = open(os.path.join(TASK, "TASK.md")).read() + "\n" + directive
        label = f"{config}-{datetime.now(timezone.utc).strftime('%H%M%S%f')}"
        out = subprocess.run(
            [args.agent, "-gateway", gateway, "-model", "mock-claude", "-dir", work, "-prompt", prompt,
             "-config", config, "-task", label, "-max-turns", str(max_turns), "-keep-tool-results", str(keep),
             "-max-tokens", "1024"],
            env={**os.environ, "PITWALL_KEY": key}, capture_output=True, text=True, timeout=600, check=True)
        return json.loads(out.stdout)


def task_detail(args, gateway, task_id, expect_calls):
    # Rows are written in batches every 250 ms; wait until all calls are in.
    for _ in range(50):
        d = api(gateway, "GET", f"/admin/tasks/{task_id}", args.admin_token)
        if d["task"]["calls"] >= expect_calls:
            return d
        time.sleep(0.1)
    return d


def mean(xs):
    return statistics.fmean(xs) if xs else float("nan")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--gateway", required=True)
    p.add_argument("--unguarded-gateway", help="a second gateway with the guard off, for the cost of an unguarded loop")
    p.add_argument("--admin-token", required=True)
    p.add_argument("--agent", default="bin/agent")
    p.add_argument("--runs", type=int, default=5)
    p.add_argument("--out", default="bench/results")
    args = p.parse_args()

    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S")
    user = api(args.gateway, "POST", "/admin/users", args.admin_token, {"email": f"phase4-{stamp}@example.test"})
    key_off = api(args.gateway, "POST", "/admin/keys", args.admin_token, {"user_id": user["id"], "name": "phase4 cache off"})["key"]
    key_on = api(args.gateway, "POST", "/admin/keys", args.admin_token,
                 {"user_id": user["id"], "name": "phase4 cache on", "cache_optimizer_on": True})["key"]

    # --- cache optimizer: 12 normal turns per run --------------------------
    cache_rows = []
    for optimizer, key in (("off", key_off), ("on", key_on)):
        for context, keep in (("full", 0), ("trim", 2)):
            config = f"cache-{optimizer}/{context}"
            for _ in range(args.runs):
                res = run_agent(args, args.gateway, key, "[[mock:turns=12]]", config, keep=keep)
                d = task_detail(args, args.gateway, res["task_id"], res["turns"])
                t, calls = d["task"], d["calls"]
                cache_rows.append({
                    "config": config, "optimizer": optimizer, "context": context, "task_id": t["id"],
                    "turns": res["turns"], "outcome": res["outcome"], "cost_usd": t["cost_usd"],
                    "prompt_tokens": t["prompt_tokens"], "cached_read_tokens": t["cached_read_tokens"],
                    "cache_write_tokens": t["cache_write_tokens"], "output_tokens": t["output_tokens"],
                    "max_context_tokens": t["max_context_tokens"],
                    "breakpoints_added": sum(c["breakpoints_added"] for c in calls),
                    "guard_stopped": any(c["guard_action"] == "stop" for c in calls),
                    "guard_warned": any(c["guard_action"] == "warn" for c in calls),
                })
                print(f"{config:16} {res['turns']:>2} calls  ${t['cost_usd']:.6f}  cached {t['cached_read_tokens']}/{t['prompt_tokens']}")

    # --- guard: seeded loops ----------------------------------------------------
    loop_rows = []
    targets = [("guarded", args.gateway)] + ([("unguarded", args.unguarded_gateway)] if args.unguarded_gateway else [])
    for name, gw in targets:
        for _ in range(args.runs * 2):
            res = run_agent(args, gw, key_off, "[[mock:mode=loop]]", f"loop/{name}", max_turns=30)
            d = task_detail(args, gw, res["task_id"], res["turns"])
            calls = d["calls"]
            stop_at = next((i + 1 for i, c in enumerate(calls) if c["guard_action"] == "stop"), None)
            warn_at = next((i + 1 for i, c in enumerate(calls) if c["guard_action"] == "warn"), None)
            loop_rows.append({"gateway": name, "task_id": res["task_id"], "turns": res["turns"], "outcome": res["outcome"],
                              "cost_usd": d["task"]["cost_usd"], "first_warning_call": warn_at, "stopped_at_call": stop_at})
            print(f"loop/{name:9} {res['turns']:>2} calls  ${d['task']['cost_usd']:.6f}  warn@{warn_at} stop@{stop_at}")

    write_report(args, stamp, cache_rows, loop_rows)


def write_report(args, stamp, cache_rows, loop_rows):
    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "phase4.json"), "w") as f:
        json.dump({"generated": stamp, "runs_per_config": args.runs, "cache": cache_rows, "loops": loop_rows}, f, indent=2)

    def by(config):
        return [r for r in cache_rows if r["config"] == config]

    lines = [
        "# Phase 4 measurements",
        "",
        f"Generated {stamp} UTC by `bench/experiments/phase4.py`, {args.runs} runs per cache configuration.",
        "",
        "**Read this first:** these runs use the mock provider, not a real model. The mock simulates",
        "Anthropic's prompt cache (writes at breakpoints, 20-position lookback, 1,024-token minimum) and",
        "plays a scripted agent: 12 tool calls cycling through the agent's tools, or, for seeded loops, the",
        "same failing test run every turn. Costs use the mock's price row ($3 input, $3.75 cache write,",
        "$0.30 cache read, $15 output per million tokens). The numbers measure the gateway's mechanisms;",
        "real-model numbers come from the Phase 5 benchmark.",
        "",
        "## Cache optimizer",
        "",
        "| Configuration | Runs | Calls per task | Cost per task | Cached share of input | Breakpoints added per task |",
        "|---|---:|---:|---:|---:|---:|",
    ]
    for config in ("cache-off/full", "cache-on/full", "cache-off/trim", "cache-on/trim"):
        rows = by(config)
        if not rows:
            continue
        share = sum(r["cached_read_tokens"] for r in rows) / max(1, sum(r["prompt_tokens"] for r in rows))
        lines.append(f"| {config} | {len(rows)} | {mean([r['turns'] for r in rows]):.1f} | ${mean([r['cost_usd'] for r in rows]):.6f} "
                     f"| {100 * share:.1f}% | {mean([r['breakpoints_added'] for r in rows]):.1f} |")
    lines.append("")
    for context in ("full", "trim"):
        off, on = by(f"cache-off/{context}"), by(f"cache-on/{context}")
        if off and on:
            c_off, c_on = mean([r["cost_usd"] for r in off]), mean([r["cost_usd"] for r in on])
            lines.append(f"- **{context} history:** the optimizer changed cost per task by "
                         f"{100 * (c_on - c_off) / c_off:+.1f}% (${c_off:.6f} -> ${c_on:.6f}).")
    lines += ["", "## Runaway-agent guard", ""]
    guarded = [r for r in loop_rows if r["gateway"] == "guarded"]
    unguarded = [r for r in loop_rows if r["gateway"] == "unguarded"]
    caught = [r for r in guarded if r["stopped_at_call"]]
    normal = cache_rows
    false_stops = [r for r in normal if r["guard_stopped"]]
    false_warns = [r for r in normal if r["guard_warned"]]
    lines += [
        "| Measure | Result |",
        "|---|---|",
        f"| Seeded loops caught (stopped by the guard) | {len(caught)} of {len(guarded)} ({100 * len(caught) / max(1, len(guarded)):.0f}%) |",
        f"| Call at which the guard first warned / stopped | {mean([r['first_warning_call'] for r in caught if r['first_warning_call']]):.1f} / {mean([r['stopped_at_call'] for r in caught]):.1f} |",
        f"| Normal runs stopped (false stops) | {len(false_stops)} of {len(normal)} ({100 * len(false_stops) / max(1, len(normal)):.0f}%) |",
        f"| Normal runs warned | {len(false_warns)} of {len(normal)} |",
        f"| Cost of a caught loop | ${mean([r['cost_usd'] for r in caught]):.6f} |",
    ]
    if unguarded:
        lines.append(f"| Cost of the same loop with the guard off (runs to the 30-turn limit) | ${mean([r['cost_usd'] for r in unguarded]):.6f} "
                     f"over {mean([r['turns'] for r in unguarded]):.0f} calls |")
    lines += [
        "",
        "## Limits of this measurement",
        "",
        "- The mock's agent behavior is scripted. A real agent's normal runs include patterns the",
        "  scripted runs don't (polling, re-running tests after a flaky failure), which is where real",
        "  false stops would come from; the Phase 5 benchmark measures those with real models.",
        "- The agent's system prompt and tool definitions are small (well under the 1,024-token",
        "  minimum), so in these runs only the growing conversation is cached. Agents with large",
        "  system prompts (Claude Code's is about 8,000 tokens) gain more from the first breakpoint.",
        "- One task, one model. Savings depend on how many turns a task takes and how fast its",
        "  context grows.",
        "",
    ]
    with open(os.path.join(args.out, "phase4.md"), "w") as f:
        f.write("\n".join(lines))
    print("\n".join(lines))


if __name__ == "__main__":
    main()
