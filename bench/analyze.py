#!/usr/bin/env python3
"""Turns benchmark runs (runs.csv from bench/run.py) into a results table.

For each configuration it reports the solve rate with a 95% Wilson interval,
the cost and tokens per solved task (with a 95% bootstrap interval over
tasks), how much of the prompt was read from cache, wall time, guard stops,
and runs where the agent edited the tests. It also checks Pitwall's task
inference against the benchmark's ground truth: each run sent an explicit
x-pitwall-task header, and every call also got an inferred task ID, so a run
is grouped correctly when all its calls share one inferred ID that no other
run has.

Writes <out>/results.md, <out>/summary.json and SVG charts in <out>/charts/.
Only the standard library is used.

    python3 bench/analyze.py bench/results/dry-run
    python3 bench/analyze.py bench/results/a bench/results/b --out bench/results/combined
"""

import argparse
import csv
import json
import math
import os
import random
import statistics
from collections import defaultdict
from datetime import datetime, timezone

NUM = ["cost_usd", "prompt_tokens", "input_tokens", "cached_read_tokens", "cache_write_tokens", "output_tokens",
       "repeated_input_tokens", "max_context_tokens", "agent_seconds", "gateway_seconds", "turns", "calls",
       "failed_calls", "guard_events", "guard_stopped", "tests_modified", "repeat", "cache_optimizer",
       "keep_tool_results", "trim_batch"]


def load(dirs):
    rows = []
    for d in dirs:
        with open(os.path.join(d, "runs.csv")) as f:
            for r in csv.DictReader(f):
                for k in NUM:
                    r[k] = float(r[k]) if r.get(k) not in (None, "") else 0.0
                rows.append(r)
    return rows


def wilson(k, n, z=1.96):
    if n == 0:
        return float("nan"), float("nan")
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return max(0.0, c - h), min(1.0, c + h)


def per_solved(runs, value):
    solved = sum(1 for r in runs if r["outcome"] == "solved")
    total = sum(value(r) for r in runs)
    return total / solved if solved else float("inf")


def bootstrap(runs, value, samples=2000, seed=7):
    """95% interval of sum(value)/solved, resampling tasks (with all their repeats)."""
    by_task = defaultdict(list)
    for r in runs:
        by_task[r["task"]].append(r)
    tasks = sorted(by_task)
    if len(tasks) < 2:
        return float("nan"), float("nan")
    rng = random.Random(seed)
    stats = []
    for _ in range(samples):
        pick = [r for t in rng.choices(tasks, k=len(tasks)) for r in by_task[t]]
        stats.append(per_solved(pick, value))
    stats.sort()
    return stats[int(0.025 * samples)], stats[int(0.975 * samples) - 1]


def tokens(r):
    return r["prompt_tokens"] + r["output_tokens"]


def summarize(rows):
    configs = list(dict.fromkeys(r["config"] for r in rows))
    out = []
    for c in configs:
        runs = [r for r in rows if r["config"] == c]
        solved = [r for r in runs if r["outcome"] == "solved"]
        n, k = len(runs), len(solved)
        lo, hi = wilson(k, n)
        prompt = sum(r["prompt_tokens"] for r in runs)
        s = {
            "config": c, "model": runs[0]["model"], "cache_optimizer": bool(runs[0]["cache_optimizer"]),
            "keep_tool_results": int(runs[0]["keep_tool_results"]),
            "trim_batch": int(runs[0]["trim_batch"] or 1),
            "runs": n, "solved": k, "solve_rate": k / n if n else float("nan"), "solve_rate_ci": [lo, hi],
            "stopped_by_guard": sum(1 for r in runs if r["guard_stopped"]),
            "warned_not_stopped": sum(1 for r in runs if r["guard_events"] and not r["guard_stopped"]),
            "calls_before_stop": statistics.fmean(r["calls"] for r in runs if r["guard_stopped"])
            if any(r["guard_stopped"] for r in runs) else float("nan"),
            "cost_of_stopped_usd": sum(r["cost_usd"] for r in runs if r["guard_stopped"]),
            "tests_modified": sum(1 for r in runs if r["tests_modified"]),
            "errors": sum(1 for r in runs if r["error"] and not r["guard_stopped"]),
            "cost_usd": sum(r["cost_usd"] for r in runs),
            "cost_per_run_usd": statistics.fmean(r["cost_usd"] for r in runs) if runs else float("nan"),
            "cost_per_solved_usd": per_solved(runs, lambda r: r["cost_usd"]),
            "cost_per_solved_ci": list(bootstrap(runs, lambda r: r["cost_usd"])),
            "tokens_per_solved": per_solved(runs, tokens),
            "tokens_per_solved_ci": list(bootstrap(runs, tokens)),
            "per_solved_split": {
                "input": per_solved(runs, lambda r: r["input_tokens"]),
                "cache_read": per_solved(runs, lambda r: r["cached_read_tokens"]),
                "cache_write": per_solved(runs, lambda r: r["cache_write_tokens"]),
                "output": per_solved(runs, lambda r: r["output_tokens"]),
            },
            "cache_read_share": sum(r["cached_read_tokens"] for r in runs) / prompt if prompt else 0.0,
            "repeated_share": sum(r["repeated_input_tokens"] for r in runs) / prompt if prompt else 0.0,
            "mean_turns_solved": statistics.fmean(r["turns"] for r in solved) if solved else float("nan"),
            "median_seconds": statistics.median(r["agent_seconds"] for r in runs) if runs else float("nan"),
            "median_seconds_solved": statistics.median(r["agent_seconds"] for r in solved) if solved else float("nan"),
            "max_context_tokens": max((r["max_context_tokens"] for r in runs), default=0),
        }
        out.append(s)
    return out


def paired(rows, baseline):
    """Each config against the baseline on the runs both made (same task, same repeat)."""
    cost = {(r["config"], r["task"], r["repeat"]): r for r in rows}
    out = []
    for c in dict.fromkeys(r["config"] for r in rows):
        if c == baseline:
            continue
        keys = [(t, rep) for (cc, t, rep) in cost if cc == c and (baseline, t, rep) in cost]
        if not keys:
            continue
        a = [cost[(c, t, rep)] for t, rep in keys]
        b = [cost[(baseline, t, rep)] for t, rep in keys]
        ca, cb = sum(r["cost_usd"] for r in a), sum(r["cost_usd"] for r in b)
        out.append({
            "config": c, "baseline": baseline, "pairs": len(keys),
            "cost_change": ca / cb - 1 if cb else float("nan"),
            "cheaper_runs": sum(1 for x, y in zip(a, b) if x["cost_usd"] < y["cost_usd"]),
            "solved_only_here": sum(1 for x, y in zip(a, b) if x["outcome"] == "solved" and y["outcome"] != "solved"),
            "solved_only_baseline": sum(1 for x, y in zip(a, b) if y["outcome"] == "solved" and x["outcome"] != "solved"),
        })
    return out


def inference(rows):
    """Compares inferred task IDs with the header ground truth."""
    owners = defaultdict(set)
    for r in rows:
        for tid in filter(None, r["inferred_task_ids"].split(";")):
            owners[tid].add(r["label"])
    checked = correct = split = merged = 0
    for r in rows:
        ids = [t for t in r["inferred_task_ids"].split(";") if t]
        if not ids:
            continue
        checked += 1
        if len(ids) > 1:
            split += 1
        if any(len(owners[t]) > 1 for t in ids):
            merged += 1
        if len(ids) == 1 and len(owners[ids[0]]) == 1:
            correct += 1
    return {"runs_checked": checked, "grouped_correctly": correct, "split": split, "merged": merged,
            "accuracy": correct / checked if checked else float("nan")}


# --- formatting -----------------------------------------------------------------

def usd(x):
    if x == float("inf"):
        return "n/a"
    if math.isnan(x):
        return "-"
    if x == 0:
        return "$0"
    return f"${x:.4f}" if x < 1 else f"${x:.2f}"


def kt(x):
    if x == float("inf"):
        return "n/a"
    if x == 0:
        return "0"
    return f"{x / 1000:.1f}k"


def pct(x):
    return "-" if math.isnan(x) else f"{100 * x:.0f}%"


def interval(lo, hi, fmt):
    if math.isnan(lo):
        return ""
    return f" ({fmt(lo)} to {fmt(hi)})"


def markdown(summary, rows, inf, plans, pairs):
    tasks = sorted({r["task"] for r in rows})
    configs = [s["config"] for s in summary]
    real = any(not s["model"].startswith("mock-") for s in summary)
    lines = [f"# Benchmark results: {', '.join(plans)}", ""]
    if not real:
        lines += ["> **Dry run on the mock provider.** Outcomes were scripted: on each task and repeat the mock",
                  "> replays the reference fix, loops, rewrites the tests, or gives up, the same choice for every",
                  "> configuration. Solve rates therefore say nothing about any model; token counts, costs, cache",
                  "> use and guard stops are what the gateway measured for those scripted runs.", ""]
    lines += [f"{len(rows)} runs: {len(tasks)} tasks x {len(configs)} configurations, "
              f"{int(max(r['repeat'] for r in rows))} repeats. Generated {datetime.now(timezone.utc):%Y-%m-%d %H:%M} UTC "
              "by `bench/analyze.py`.", ""]
    lines += ["## Summary", "",
              "| Config | Model | Cache opt. | Tool results kept | Solved | Solve rate (95% CI) | Cost per solved task (95% CI) "
              "| Tokens per solved task | Cache read share | Median time | Guard stops | Tests edited |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for s in summary:
        lo, hi = s["solve_rate_ci"]
        clo, chi = s["cost_per_solved_ci"]
        keep = "all" if s["keep_tool_results"] == 0 else f"last {s['keep_tool_results']}"
        if s["keep_tool_results"] and s["trim_batch"] > 1:
            keep += f", trimmed {s['trim_batch']} at a time"
        lines.append(
            f"| {s['config']} | `{s['model']}` | {'on' if s['cache_optimizer'] else 'off'} | {keep} "
            f"| {s['solved']}/{s['runs']} | {pct(s['solve_rate'])}{interval(lo, hi, pct)} "
            f"| {usd(s['cost_per_solved_usd'])}{interval(clo, chi, usd)} | {kt(s['tokens_per_solved'])} "
            f"| {pct(s['cache_read_share'])} | {s['median_seconds']:.0f}s | {s['stopped_by_guard']} | {s['tests_modified']} |")
    lines += ["", "Cost per solved task is the configuration's total spend divided by the tasks it solved, so failed",
              "and stopped runs count against it. Its interval resamples tasks (with all their repeats). A solve",
              "is checked by the runner with the task's original tests restored.", ""]

    if pairs:
        lines += [f"## Paired against {pairs[0]['baseline']}", "",
                  "The same task and repeat under two configurations, so task difficulty cancels out.", "",
                  "| Config | Pairs | Total cost change | Cheaper on | Solved only here | Solved only by the baseline |",
                  "|---|---|---|---|---|---|"]
        for p in pairs:
            change = "-" if math.isnan(p["cost_change"]) else f"{100 * p['cost_change']:+.1f}%"
            lines.append(f"| {p['config']} | {p['pairs']} | {change} | {p['cheaper_runs']}/{p['pairs']} "
                         f"| {p['solved_only_here']} | {p['solved_only_baseline']} |")
        lines.append("")

    lines += ["## Charts", "",
              "![Cost per solved task](charts/cost_per_solved.svg)", "",
              "![Solve rate against cost per solved task](charts/frontier.svg)", "",
              "![Where the tokens of a solved task go](charts/tokens.svg)", ""]

    lines += ["## Spend", "", "| Config | Runs | Total cost | Cost per run | Uncached input per solved | Cache read per solved "
              "| Cache write per solved | Output per solved | Mean turns (solved) | Largest prompt | Runs with an API error |",
              "|---|---|---|---|---|---|---|---|---|---|---|"]
    for s in summary:
        sp = s["per_solved_split"]
        lines.append(f"| {s['config']} | {s['runs']} | {usd(s['cost_usd'])} | {usd(s['cost_per_run_usd'])} "
                     f"| {kt(sp['input'])} | {kt(sp['cache_read'])} | {kt(sp['cache_write'])} | {kt(sp['output'])} "
                     f"| {s['mean_turns_solved']:.1f} | {kt(s['max_context_tokens'])} | {s['errors']} |")
    lines.append("")

    lines += ["## Solved runs per task", "", "| Task | " + " | ".join(configs) + " |",
              "|---|" + "---|" * len(configs)]
    for t in tasks:
        cells = []
        for c in configs:
            rr = [r for r in rows if r["task"] == t and r["config"] == c]
            cells.append(f"{sum(1 for r in rr if r['outcome'] == 'solved')}/{len(rr)}")
        lines.append(f"| {t} | " + " | ".join(cells) + " |")
    lines.append("")

    lines += ["## Runaway guard", "",
              "Runs the guard stopped, how many calls they made (the refused call included) and what they cost.", "",
              "| Config | Runs stopped | Mean calls when stopped | Cost of stopped runs | Warned, not stopped |",
              "|---|---|---|---|---|"]
    for s in summary:
        calls = "-" if math.isnan(s["calls_before_stop"]) else f"{s['calls_before_stop']:.1f}"
        lines.append(f"| {s['config']} | {s['stopped_by_guard']} | {calls} | {usd(s['cost_of_stopped_usd'])} "
                     f"| {s['warned_not_stopped']} |")
    lines.append("")

    lines += ["## Task inference", "",
              "Every run sent its own `x-pitwall-task` header, which is the ground truth. Pitwall also inferred a",
              "task for every call from the conversation alone, as it does for clients that send no header.", "",
              f"- Runs checked: {inf['runs_checked']}",
              f"- Grouped correctly (one inferred task, not shared with another run): {inf['grouped_correctly']} "
              f"({pct(inf['accuracy'])})",
              f"- Split across several inferred tasks: {inf['split']}",
              f"- Merged with another run: {inf['merged']}", ""]
    return "\n".join(lines)


# --- SVG charts -------------------------------------------------------------------

STYLE = """<style>
  .bg { fill: #fcfcfb } .ink { fill: #0b0b0b } .ink2 { fill: #52514e } .muted { fill: #898781 }
  .grid { stroke: #e1e0d9 } .axis { stroke: #c3c2b7 } .whisk { stroke: #52514e } .ring { stroke: #fcfcfb }
  .s1 { fill: #2a78d6 } .s2 { fill: #eb6834 } .s3 { fill: #1baf7a } .s4 { fill: #eda100 }
  text { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; font-size: 12px }
  .title { font-size: 14px; font-weight: 600 }
  @media (prefers-color-scheme: dark) {
    .bg { fill: #1a1a19 } .ink { fill: #f0efec } .ink2 { fill: #c3c2b7 }
    .grid { stroke: #2c2c2a } .axis { stroke: #383835 } .whisk { stroke: #c3c2b7 } .ring { stroke: #1a1a19 }
    .s1 { fill: #3987e5 } .s2 { fill: #d95926 } .s3 { fill: #199e70 } .s4 { fill: #c98500 }
  }
</style>"""


def esc(s):
    return str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def svg(width, height, body, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}" '
            f'role="img" aria-label="{esc(title)}">{STYLE}<rect class="bg" width="{width}" height="{height}" rx="8"/>'
            f'{body}</svg>\n')


def hbar(x0, x1, y, h, cls, round_end=True, tip=""):
    """A horizontal bar, square at the baseline with a 4px rounded data end."""
    w = x1 - x0
    if w <= 0:
        return ""
    r = min(4, w, h / 2) if round_end else 0
    d = (f"M{x0:.1f},{y:.1f} H{x1 - r:.1f} A{r},{r} 0 0 1 {x1:.1f},{y + r:.1f} V{y + h - r:.1f} "
         f"A{r},{r} 0 0 1 {x1 - r:.1f},{y + h:.1f} H{x0:.1f} Z")
    t = f"<title>{esc(tip)}</title>" if tip else ""
    return f'<path class="{cls}" d="{d}">{t}</path>'


def nice_max(v):
    if v <= 0 or math.isinf(v) or math.isnan(v):
        return 1.0
    e = 10 ** math.floor(math.log10(v))
    for m in (1, 2, 2.5, 5, 10):
        if v <= m * e:
            return m * e
    return 10 * e


def ticks(vmax, n=4):
    return [vmax * i / n for i in range(n + 1)]


def chart_cost(summary):
    left, right, top, row = 190, 110, 48, 34
    width = 760
    plot = width - left - right
    vals = [s["cost_per_solved_usd"] for s in summary]
    his = [s["cost_per_solved_ci"][1] for s in summary]
    finite = [v for v in vals + his if not (math.isinf(v) or math.isnan(v))]
    vmax = nice_max(max(finite, default=1))
    height = top + row * len(summary) + 40
    x = lambda v: left + plot * min(v, vmax) / vmax
    b = [f'<text class="ink title" x="16" y="26">Cost per solved task</text>']
    for t in ticks(vmax):
        b.append(f'<line class="grid" x1="{x(t):.1f}" x2="{x(t):.1f}" y1="{top - 6}" y2="{height - 34}" stroke-width="1"/>')
        b.append(f'<text class="muted" x="{x(t):.1f}" y="{height - 18}" text-anchor="middle">{usd(t)}</text>')
    for i, s in enumerate(summary):
        y = top + i * row
        v = s["cost_per_solved_usd"]
        lo, hi = s["cost_per_solved_ci"]
        b.append(f'<text class="ink2" x="{left - 10}" y="{y + 15}" text-anchor="end">{esc(s["config"])}</text>')
        if math.isinf(v):
            b.append(f'<text class="muted" x="{left + 4}" y="{y + 15}">nothing solved</text>')
            continue
        b.append(hbar(left, x(v), y + 4, 16, "s1", tip=f"{s['config']}: {usd(v)} per solved task"))
        if not math.isnan(lo):
            hx = x(hi) if not math.isinf(hi) else left + plot
            b.append(f'<line class="whisk" x1="{x(lo):.1f}" x2="{hx:.1f}" y1="{y + 12}" y2="{y + 12}" stroke-width="1.5"/>')
            for cx in (x(lo), hx):
                b.append(f'<line class="whisk" x1="{cx:.1f}" x2="{cx:.1f}" y1="{y + 7}" y2="{y + 17}" stroke-width="1.5"/>')
        label_x = max(x(v), x(hi) if not (math.isnan(hi) or math.isinf(hi)) else x(v)) + 8
        b.append(f'<text class="ink" x="{label_x:.1f}" y="{y + 16}">{usd(v)}</text>')
    b.append(f'<line class="axis" x1="{left}" x2="{left}" y1="{top - 6}" y2="{height - 34}" stroke-width="1"/>')
    return svg(width, height, "".join(b), "Cost per solved task by configuration, with 95% intervals")


def chart_frontier(summary):
    width, height = 760, 420
    left, right, top, bottom = 70, 40, 48, 50
    pw, ph = width - left - right, height - top - bottom
    pts = [s for s in summary if not math.isinf(s["cost_per_solved_usd"])]
    vmax = nice_max(max((s["cost_per_solved_usd"] for s in pts), default=1) * 1.1)
    x = lambda r: left + pw * r
    y = lambda v: top + ph * (1 - v / vmax)
    b = [f'<text class="ink title" x="16" y="26">Solve rate against cost per solved task (lower right is better)</text>']
    for t in ticks(vmax):
        b.append(f'<line class="grid" x1="{left}" x2="{left + pw}" y1="{y(t):.1f}" y2="{y(t):.1f}" stroke-width="1"/>')
        b.append(f'<text class="muted" x="{left - 8}" y="{y(t) + 4:.1f}" text-anchor="end">{usd(t)}</text>')
    for r in (0, 0.25, 0.5, 0.75, 1):
        b.append(f'<text class="muted" x="{x(r):.1f}" y="{top + ph + 20}" text-anchor="middle">{int(r * 100)}%</text>')
    b.append(f'<line class="axis" x1="{left}" x2="{left + pw}" y1="{top + ph}" y2="{top + ph}" stroke-width="1"/>')
    b.append(f'<text class="ink2" x="{left + pw / 2}" y="{height - 10}" text-anchor="middle">solve rate</text>')
    placed = []
    for s in pts:
        cx, cy = x(s["solve_rate"]), y(s["cost_per_solved_usd"])
        b.append(f'<circle class="s1 ring" cx="{cx:.1f}" cy="{cy:.1f}" r="5" stroke-width="2">'
                 f'<title>{esc(s["config"])}: {pct(s["solve_rate"])} solved, {usd(s["cost_per_solved_usd"])} per solved task</title></circle>')
        ly = cy - 9
        while any(abs(ly - p[1]) < 14 and abs(cx - p[0]) < 160 for p in placed):
            ly += 14
        placed.append((cx, ly))
        anchor = "end" if cx > left + pw * 0.7 else "start"
        dx = -9 if anchor == "end" else 9
        b.append(f'<text class="ink" x="{cx + dx:.1f}" y="{ly:.1f}" text-anchor="{anchor}">{esc(s["config"])}</text>')
    return svg(width, height, "".join(b), "Solve rate against cost per solved task")


def chart_tokens(summary):
    parts = [("input", "uncached input", "s1"), ("cache_read", "cache read", "s2"),
             ("cache_write", "cache write", "s3"), ("output", "output", "s4")]
    left, right, top, row = 190, 90, 70, 34
    width = 760
    plot = width - left - right
    rows = [s for s in summary if not math.isinf(s["tokens_per_solved"])]
    vmax = nice_max(max((s["tokens_per_solved"] for s in rows), default=1))
    height = top + row * max(1, len(rows)) + 40
    x = lambda v: left + plot * v / vmax
    b = [f'<text class="ink title" x="16" y="26">Where the tokens of a solved task go</text>']
    lx = 16
    for _, name, cls in parts:  # legend
        b.append(f'<rect class="{cls}" x="{lx}" y="38" width="10" height="10" rx="2"/>')
        b.append(f'<text class="ink2" x="{lx + 15}" y="47">{name}</text>')
        lx += 30 + 7 * len(name)
    for t in ticks(vmax):
        b.append(f'<line class="grid" x1="{x(t):.1f}" x2="{x(t):.1f}" y1="{top - 6}" y2="{height - 34}" stroke-width="1"/>')
        b.append(f'<text class="muted" x="{x(t):.1f}" y="{height - 18}" text-anchor="middle">{kt(t)}</text>')
    for i, s in enumerate(rows):
        y = top + i * row
        b.append(f'<text class="ink2" x="{left - 10}" y="{y + 15}" text-anchor="end">{esc(s["config"])}</text>')
        pos = left
        segs = [(k, n, c, s["per_solved_split"][k]) for k, n, c in parts if s["per_solved_split"][k] > 0]
        for j, (_, name, cls, v) in enumerate(segs):
            end = pos + plot * v / vmax
            last = j == len(segs) - 1
            gap = 0 if last else 2  # 2px surface gap between segments
            b.append(hbar(pos, end - gap, y + 4, 16, cls, round_end=last, tip=f"{s['config']}: {kt(v)} {name}"))
            pos = end
        b.append(f'<text class="ink" x="{pos + 8:.1f}" y="{y + 16}">{kt(s["tokens_per_solved"])}</text>')
    b.append(f'<line class="axis" x1="{left}" x2="{left}" y1="{top - 6}" y2="{height - 34}" stroke-width="1"/>')
    return svg(width, height, "".join(b), "Tokens per solved task, split by kind")


def clean(v):
    if isinstance(v, float) and (math.isinf(v) or math.isnan(v)):
        return None
    if isinstance(v, dict):
        return {k: clean(x) for k, x in v.items()}
    if isinstance(v, list):
        return [clean(x) for x in v]
    return v


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("dirs", nargs="+", help="results directories holding runs.csv")
    p.add_argument("--out", help="where to write (default: the first directory)")
    p.add_argument("--baseline", help="config to pair the others against (default: the first one run)")
    args = p.parse_args()
    out = args.out or args.dirs[0]
    rows = load(args.dirs)
    if not rows:
        raise SystemExit("no runs")
    summary = summarize(rows)
    inf = inference(rows)
    plans = list(dict.fromkeys(r["plan"] for r in rows))
    pairs = paired(rows, args.baseline or rows[0]["config"])
    os.makedirs(os.path.join(out, "charts"), exist_ok=True)
    with open(os.path.join(out, "results.md"), "w") as f:
        f.write(markdown(summary, rows, inf, plans, pairs))
    with open(os.path.join(out, "summary.json"), "w") as f:
        json.dump(clean({"plans": plans, "configs": summary, "paired": pairs, "task_inference": inf}), f, indent=2)
    for name, fn in (("cost_per_solved", chart_cost), ("frontier", chart_frontier), ("tokens", chart_tokens)):
        with open(os.path.join(out, "charts", name + ".svg"), "w") as f:
            f.write(fn(summary))
    print(f"wrote {out}/results.md, summary.json and charts/")


if __name__ == "__main__":
    main()
