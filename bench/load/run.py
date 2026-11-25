#!/usr/bin/env python3
"""Runs bench/load/loadtest.js (k6) against several targets and rates, and
writes the latency table to <out>/results.md and results.json.

Each target is NAME=URL, optionally with ,KEY_ENV naming an environment
variable that holds the bearer token. The first target should be the mock
provider itself: the others are compared with it, and the difference is
what the proxy in between adds.

    python3 bench/load/run.py --out bench/results/load --rates 100,500 \\
        direct=http://127.0.0.1:9300 \\
        pitwall-dev=http://127.0.0.1:8380 \\
        pitwall-full=http://127.0.0.1:8381,PITWALL_LOAD_KEY \\
        litellm=http://127.0.0.1:4000,LITELLM_MASTER_KEY

Every measurement is preceded by a 5-second warm-up at the same rate that is
not recorded. Targets take turns within each rate, so they see the same
machine conditions.
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))


def k6(target, key, rate, duration, stream, summary):
    env = {**os.environ, "TARGET": target, "RATE": str(rate), "DURATION": duration,
           "STREAM": "1" if stream else "0", "SUMMARY": summary or "", "KEY": key}
    r = subprocess.run(["k6", "run", "--quiet", "--no-color", os.path.join(HERE, "loadtest.js")],
                       env=env, capture_output=True, text=True)
    if r.returncode not in (0, 99):  # 99: thresholds crossed, still a result
        sys.exit(f"k6 failed against {target}:\n{r.stderr[-2000:]}")
    if summary:
        with open(summary) as f:
            return json.load(f)
    return None


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("targets", nargs="+", help="NAME=URL[,KEY_ENV]")
    p.add_argument("--out", required=True)
    p.add_argument("--rates", default="100,500", help="requests per second, comma-separated")
    p.add_argument("--duration", default="30s")
    p.add_argument("--stream-rate", type=int, default=100, help="also measure streaming at this rate (0: skip)")
    p.add_argument("--delay-ms", type=float, default=50, help="the mock's fixed delay, for the table")
    args = p.parse_args()

    targets = []
    for t in args.targets:
        name, rest = t.split("=", 1)
        url, _, key_env = rest.partition(",")
        targets.append((name, url, os.environ.get(key_env, "") if key_env else ""))
    runs = [(int(r), False) for r in args.rates.split(",")]
    if args.stream_rate:
        runs.append((args.stream_rate, True))

    os.makedirs(args.out, exist_ok=True)
    results = []
    with tempfile.TemporaryDirectory() as tmp:
        for rate, stream in runs:
            for name, url, key in targets:
                k6(url, key, rate, "5s", stream, None)  # warm-up
                s = k6(url, key, rate, args.duration, stream, os.path.join(tmp, "s.json"))
                s["name"] = name
                results.append(s)
                print(f"{name:<14} {rate:>4} rps {'stream' if stream else 'json  '} "
                      f"p50 {s['ms']['p50']:7.2f} ms  p99 {s['ms']['p99']:7.2f} ms  "
                      f"failed {100 * s['failed_rate']:.2f}%  dropped {s['dropped']}")

    with open(os.path.join(args.out, "results.json"), "w") as f:
        json.dump({"delay_ms": args.delay_ms, "duration": args.duration, "runs": results}, f, indent=2)

    lines = [f"| Target | Rate | Mode | Requests | Failed | p50 (ms) | p99 (ms) | Added p50 | Added p99 |",
             "|---|---|---|---|---|---|---|---|---|"]
    for rate, stream in runs:
        group = [r for r in results if r["rate"] == rate and r["stream"] == stream]
        base = group[0]["ms"]
        for r in group:
            added = r is not group[0]
            lines.append(
                f"| {r['name']} | {rate}/s | {'stream' if stream else 'JSON'} | {r['requests']} "
                f"| {100 * r['failed_rate']:.2f}% | {r['ms']['p50']:.2f} | {r['ms']['p99']:.2f} "
                f"| {('+%.2f' % (r['ms']['p50'] - base['p50'])) if added else '-'} "
                f"| {('+%.2f' % (r['ms']['p99'] - base['p99'])) if added else '-'} |")
    with open(os.path.join(args.out, "table.md"), "w") as f:
        f.write("\n".join(lines) + "\n")
    print(f"wrote {args.out}/results.json and table.md")


if __name__ == "__main__":
    main()
