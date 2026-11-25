# Harness benchmark

The question Pitwall exists to answer: **what does it cost to finish a
task, and which model and harness setup does it cheapest?** A per-request
bill can't say, because a cheap model that needs three tries, or a long
context that gets resent every turn, only shows up per task.

The benchmark runs a fixed set of coding tasks under several configurations
(model, prompt caching, how much tool output the agent keeps), several times
each, through the gateway. Pitwall groups each run's calls into a task and
prices it; the runner checks the result itself. The headline number is
**cost per solved task**: a configuration's total spend divided by the tasks
it solved, so failed and stopped runs count against it.

## Tasks

Twenty small Go repositories in [`tasks/`](tasks), each with failing tests
and a `TASK.md` that says what is wrong, standard library only. The agent
gets a copy of one task directory; the reference fixes live in
[`solutions/`](solutions), outside what the agent can read.

| Task | What has to be fixed | Files |
|---|---|---|
| 001-word-count | count words separated by any whitespace | 1 |
| 002-roman-numerals | subtractive numerals (IV, XC, CM) | 1 |
| 003-binary-search | loop bound that misses the last candidate | 1 |
| 004-interval-merge | unsorted input, touching intervals, don't modify the input | 1 |
| 005-lru-cache | reads and updates must count as use | 1 |
| 006-semver-compare | numeric parts, `v` prefix, pre-release precedence | 1 |
| 007-csv-line | quoted fields, escaped quotes, unterminated quote error | 1 |
| 008-split-bill | splitting cents exactly; formatting negative amounts | 1 |
| 009-word-wrap | whitespace runs, long words, no trailing spaces | 1 |
| 010-luhn | right-to-left doubling, spaces, invalid characters | 1 |
| 011-retry-backoff | cap at the maximum, overflow, negative attempts | 1 |
| 012-token-bucket | fractional refill and the capacity cap, with a fake clock | 1 |
| 013-brackets | nesting by type; brackets inside string literals | 1 |
| 014-template | `{{ name }}` parsing, missing values, no re-expansion | 1 |
| 015-topo-sort | nodes that only appear as dependencies, stable order, cycles | 1 |
| 016-config-merge | deep merge, nil deletes, results that share nothing with the inputs | 2 |
| 017-inventory | three bugs across two files: over-reservation, partial orders, cancel | 2 |
| 018-duration-parse | units including `d` and `ms`, decimals, spaces, errors | 1 |
| 019-percentile | interpolation, p=100, don't sort the caller's slice | 1 |
| 020-markdown-table | widths in characters, short rows, minimum width, escaped pipes | 1 |

`python3 bench/check_tasks.py` (CI runs it) checks that every task fails as
shipped and passes, three times over, once its reference fix is applied.

## How a run works

[`run.py`](run.py) drives Pitwall's own agent ([`agent/`](../agent/agent.go):
list, read, write and edit files, run the tests; no shell) on a scratch copy
of the task:

1. The agent calls the model through the gateway with `x-pitwall-task` set
   to a unique label and `x-pitwall-config` to the configuration's name.
2. When the agent stops, the runner puts back the task's original test files
   (and deletes any new ones) and runs `go test ./...` itself. An agent that
   edits the tests to pass gets no credit, and the runs where it tried are
   counted.
3. The runner reports that verified outcome to the gateway, so the task's
   dashboard row agrees, then reads the task's cost, tokens, cache use and
   guard events from the admin API.
4. It appends one row to `runs.csv` and saves the agent's diff under
   `diffs/<config>/`.

Each configuration gets its own virtual key (so caching can be on for one
and off for another), and all keys belong to one new team whose monthly
budget is the plan's `budget_usd`. That makes the budget a hard cap enforced
by the gateway: once it is spent, calls get a 402 and the runner stops. Runs
already in `runs.csv` are skipped, so an interrupted benchmark resumes with
new keys under the same team. Runs go repeat by repeat, every configuration
on a task before the next task, so a budget stop leaves the configurations
evenly covered.

## Plans

A plan is a JSON file in [`plans/`](plans):

| Field | Meaning |
|---|---|
| `name` | labels the runs; one results directory per plan |
| `gateway` | gateway URL (`--gateway` overrides it) |
| `repeats`, `tasks` | repeats per task; task name globs |
| `max_turns`, `max_tokens` | agent limits per run and per call |
| `budget_usd` | total spend cap for the plan; required for paid models |
| `configs[]` | `label`, `model`, `cache_optimizer`, `keep_tool_results` (0 keeps the full history), `trim_batch` (move the trim point this many results at a time), `mock` (dry runs only) |

- [`dry-run.json`](plans/dry-run.json): the whole pipeline on the mock
  provider (below).
- [`claude.json`](plans/claude.json): Haiku, Sonnet and Opus, and four
  harness setups on Sonnet; 20 tasks x 6 configurations x 3 repeats.
  [Results](results/claude/results.md) (360 runs, $4.98).

What the two runs found: [`results/findings.md`](results/findings.md).

## Running it

Start the stack (`docker compose up -d`, which runs the gateway with
Postgres, Redis and the mock), then:

```bash
make build
export PITWALL_ADMIN_TOKEN=...   # the gateway's admin token
python3 bench/run.py --plan bench/plans/dry-run.json --out bench/results/dry-run --jobs 4
python3 bench/analyze.py bench/results/dry-run
```

For real models, the gateway needs the provider key in its environment
(`ANTHROPIC_API_KEY` in `.env`); the runner never sees it. Check the plan's
`budget_usd` first: it is the most the benchmark can spend.

```bash
python3 bench/run.py --plan bench/plans/claude.json --out bench/results/claude --jobs 4
python3 bench/analyze.py bench/results/claude
```

`--limit N` runs only the next N runs, a cheap way to check a plan before
the whole thing.

## The report

[`analyze.py`](analyze.py) writes `results.md`, `summary.json` and three
SVG charts. Per configuration: solve rate with a 95% Wilson interval; cost
and tokens per solved task with a 95% bootstrap interval (tasks resampled
with all their repeats); the token split into uncached input, cache reads,
cache writes and output; median wall time; guard stops and their cost; runs
where the agent edited the tests. A paired table compares every
configuration with the baseline on the same task and repeat, which cancels
out task difficulty. It also scores Pitwall's task inference: each run's
header is ground truth, and every call also gets an inferred task from the
conversation alone.

## Dry run on the mock

The mock provider can't write code, so for the dry run the runner scripts
it with `[[mock:script=...]]`: on each task and repeat it either replays the
reference fix (after a first change that doesn't help), loops on the same
failing test run, rewrites the tests so they pass, or looks around and gives
up. The choice depends on the task and repeat only, so every configuration
gets the same scripts and the differences between them come from the
configuration alone. Solve rates in a dry run are set by the script, not
measured; tokens, cost, caching, guard stops and inference are measured.
Results: [`results/dry-run/results.md`](results/dry-run/results.md); what
they show: [`results/findings.md`](results/findings.md).

## Load test

[`load/loadtest.js`](load/loadtest.js) is a k6 script for gateway overhead:
a fixed rate of chat completions against the mock provider with a fixed
delay, sent directly and through each proxy. [`load/run.py`](load/run.py)
runs it for several targets and rates:

```bash
bin/mockprovider -addr 127.0.0.1:9300 -delay 50ms &
MOCK_BASE_URL=http://127.0.0.1:9300 bin/gateway &    # dev mode, on :8080
python3 bench/load/run.py --out /tmp/load --rates 100,500 \
    direct=http://127.0.0.1:9300 pitwall=http://127.0.0.1:8080
```

Add `,KEY_ENV` to a target to send the token in that environment variable
(a virtual key for a gateway with a database).

Results, with LiteLLM as a baseline: [`results/load`](results/load/results.md).

## Caveats

- The tasks are small, single-package Go fixes; long contexts and large
  repositories, where caching and trimming matter most, are not covered.
- Every configuration reaches the provider through the gateway's one API
  key, so configurations on the same model can read each other's cached
  tools and system prompt. Agents sharing a gateway would behave the same.
- Three repeats per task give wide intervals on solve rates; the paired
  table is the more sensitive comparison.
- Pitwall's own agent is deliberately plain. Agents with planning, a shell
  or their own context management would score and spend differently.
