#!/usr/bin/env python3
"""Pitwall in two minutes: an agent fixes a bug through the gateway, the task
appears with its cost, and a seeded loop gets stopped by the guard.

Needs a running stack (docker compose up -d) with a provider key for the
agent's model in .env, the agent binary (make build) and the admin token:

    PITWALL_ADMIN_TOKEN=... python3 scripts/demo.py
    PITWALL_ADMIN_TOKEN=... python3 scripts/demo.py --record docs/demo.svg

It creates a key for the demo and revokes it at the end. --record also
writes an animated SVG of the terminal output. Standard library only.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class Screen:
    """Prints lines and remembers when each appeared, for --record."""

    def __init__(self, pace):
        self.start, self.lines, self.pace = time.monotonic(), [], pace

    def out(self, text="", kind="out"):
        print(text, flush=True)
        self.lines.append((time.monotonic() - self.start, text, kind))
        time.sleep(self.pace)

    def cmd(self, text):
        self.out("$ " + text, "cmd")

    def note(self, text):
        self.out("# " + text, "note")


def api(args, method, path, body=None, token=None):
    req = urllib.request.Request(args.gateway + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + (token or args.admin_token),
                                          "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def run_agent(args, key, model, task_dir, label, prompt=None, max_turns=30):
    with tempfile.TemporaryDirectory() as tmp:
        work = os.path.join(tmp, os.path.basename(task_dir))
        shutil.copytree(task_dir, work)
        cmd = [args.agent, "-gateway", args.gateway, "-model", model, "-dir", work, "-task", label,
               "-config", "demo", "-max-turns", str(max_turns)]
        if prompt:
            cmd += ["-prompt", prompt]
        p = subprocess.run(cmd, env={**os.environ, "PITWALL_KEY": key}, capture_output=True, text=True, timeout=600)
        return json.loads(p.stdout)


def task_detail(args, task_id, calls):
    for _ in range(50):  # calls are written in batches
        d = api(args, "GET", f"/admin/tasks/{task_id}")
        if d["task"]["calls"] >= calls:
            return d
        time.sleep(0.1)
    return d


def show_calls(sc, d):
    sc.out("  call  prompt  cached  output     cost  guard")
    for i, c in enumerate(d["calls"], 1):
        guard = c.get("guard_signal") or ""  # e.g. repeated_exchange:warn
        if c.get("status", 200) >= 400:
            sc.out(f"  {i:>4}  {'-':>6}  {'-':>6}  {'-':>6}  {'-':>7}  {guard}  (refused, status {c['status']})")
            continue
        sc.out(f"  {i:>4}  {c['context_tokens']:>6}  {c['cached_read_tokens']:>6}  {c['output_tokens']:>6}"
               f"  ${c['cost_usd']:.4f}  {guard}")


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--gateway", default=os.environ.get("PITWALL_GATEWAY_URL", "http://localhost:8080"))
    p.add_argument("--admin-token", default=os.environ.get("PITWALL_ADMIN_TOKEN", ""))
    p.add_argument("--agent", default=os.path.join(ROOT, "bin", "agent"))
    p.add_argument("--model", default="claude-haiku-5-5", help="the model the agent uses for the real task")
    p.add_argument("--task", default="006-semver-compare", help="a task from bench/tasks")
    p.add_argument("--pace", type=float, default=0.25, help="seconds between printed lines")
    p.add_argument("--record", help="also write an animated SVG of the output here")
    args = p.parse_args()
    if not args.admin_token:
        sys.exit("set PITWALL_ADMIN_TOKEN")

    sc = Screen(args.pace)
    stamp = datetime.now(timezone.utc).strftime("%H%M%S")
    user = api(args, "POST", "/admin/users", {"email": f"demo+{stamp}@pitwall.local", "role": "dev"})
    k = api(args, "POST", "/admin/keys", {"user_id": user["id"], "name": "demo", "cache_optimizer_on": True})
    key = k["key"]
    try:
        sc.note("Pitwall sits between coding agents and model providers.")
        sc.note(f"1. An agent fixes a failing test through it, on {args.model}.")
        task_dir = os.path.join(ROOT, "bench", "tasks", args.task)
        label = f"demo-{args.task}-{stamp}"
        sc.cmd(f"cat bench/tasks/{args.task}/TASK.md | head -3")
        with open(os.path.join(task_dir, "TASK.md")) as f:
            for line in f.read().splitlines()[:3]:
                sc.out("  " + line)
        sc.cmd(f"bin/agent -model {args.model} -dir {args.task} -task {label}")
        res = run_agent(args, key, args.model, task_dir, label)
        sc.out(f"  {res['outcome']} in {res['turns']} calls, {res['seconds']:.1f}s"
               f"   x-pitwall-task-id: {res['task_id']}")

        sc.note("2. Pitwall grouped those calls into one task and priced it.")
        sc.cmd(f"curl $PITWALL/admin/tasks/{res['task_id']}")
        d = task_detail(args, res["task_id"], res["turns"])
        t = d["task"]
        sc.out(f"  outcome {t['outcome']}  calls {t['calls']}  cost ${t['cost_usd']:.4f}  "
               f"context {t['first_context_tokens']} -> {t['max_context_tokens']} tokens  "
               f"repeated input {100 * t['repeated_share']:.0f}%")
        show_calls(sc, d)

        sc.note("3. A seeded loop: the agent runs the same failing test, again and again.")
        loop_label = f"demo-loop-{stamp}"
        with open(os.path.join(task_dir, "TASK.md")) as f:
            prompt = f.read() + "\n[[mock:mode=loop]]\n"
        sc.cmd(f"bin/agent -model mock-claude -dir {args.task} -task {loop_label}   # mock: loops on purpose")
        loop = run_agent(args, key, "mock-claude", task_dir, loop_label, prompt=prompt)
        msg = loop.get("error", "")
        msg = msg[msg.find("Pitwall stopped"):] if "Pitwall stopped" in msg else msg
        sc.out(f"  {loop['outcome']} after {loop['turns']} calls")
        for chunk in wrap(msg.split('"}')[0], 96):
            sc.out("  " + chunk)
        d = task_detail(args, loop["task_id"], loop["turns"])
        show_calls(sc, d)

        sc.note("4. Both runs, as tasks: what each one cost and how it ended.")
        sc.cmd(f'curl "$PITWALL/admin/tasks?key_id={k["id"]}"')
        rows = api(args, "GET", f"/admin/tasks?key_id={k['id']}")["data"]
        for r in rows:
            sc.out(f"  {r['external_id'] or r['id']:<34} {r['outcome']:<8} {r['calls']:>2} calls  ${r['cost_usd']:.4f}"
                   f"  guard events {r['guard_events']}")
        sc.note("The same tasks are in Grafana (localhost:3001) and Langfuse (localhost:3000).")
    finally:
        api(args, "DELETE", f"/admin/keys/{k['id']}")

    if args.record:
        with open(args.record, "w") as f:
            f.write(to_svg(sc.lines))
        print(f"wrote {args.record}", file=sys.stderr)


def wrap(text, width):
    words, line, out = text.split(), "", []
    for w in words:
        if line and len(line) + 1 + len(w) > width:
            out.append(line)
            line = w
        else:
            line = f"{line} {w}".strip()
    return out + ([line] if line else [])


def to_svg(lines, rows=22, cols=108, hold=6.0):
    """An animated terminal: lines appear when they were printed, and the
    view scrolls once the window is full. Loops after a pause."""
    cw, lh, pad, bar = 7.8, 18, 14, 28
    width, height = int(cols * cw + 2 * pad), int(rows * lh + 2 * pad + bar)
    total = lines[-1][0] + hold
    pct = lambda t: f"{100 * t / total:.3f}"
    css = [f"@keyframes s {{ 0% {{ transform: translateY(0) }}"]
    for i, (t, _, _) in enumerate(lines):
        if i >= rows:
            css.append(f" {pct(t)}% {{ transform: translateY({-(i - rows + 1) * lh}px) }}")
    css.append(" 100% { transform: translateY(" + str(-max(0, len(lines) - rows) * lh) + "px) } }")
    body = []
    for i, (t, text, kind) in enumerate(lines):
        css.append(f"@keyframes l{i} {{ 0%, {pct(max(0, t - 0.01))}% {{ opacity: 0 }} {pct(t)}%, 100% {{ opacity: 1 }} }}"
                   f" .l{i} {{ animation: l{i} {total:.2f}s infinite }}")
        esc = text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        body.append(f'<text class="{kind} l{i}" x="{pad}" y="{pad + bar + (i + 1) * lh - 5}" xml:space="preserve">{esc}</text>')
    style = ("text { font: 13px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #d7d7d2 }"
             " .cmd { fill: #8fd18f } .note { fill: #e0b46b } "
             f".scroll {{ animation: s {total:.2f}s infinite; animation-timing-function: step-end }} " + "".join(css))
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}"'
            f' role="img" aria-label="Pitwall demo: an agent solves a task, the task is priced, a loop is stopped">'
            f'<style>{style}</style>'
            f'<rect width="{width}" height="{height}" rx="10" fill="#1c1c1b"/>'
            f'<circle cx="18" cy="15" r="5" fill="#e5534b"/><circle cx="34" cy="15" r="5" fill="#d29922"/>'
            f'<circle cx="50" cy="15" r="5" fill="#57ab5a"/>'
            f'<clipPath id="v"><rect x="0" y="{bar}" width="{width}" height="{height - bar}"/></clipPath>'
            f'<g clip-path="url(#v)"><g class="scroll">{"".join(body)}</g></g></svg>\n')


if __name__ == "__main__":
    main()
