# 0005: Harness benchmark

## Context

Pitwall's claim is that cost per task, not cost per request, is what tells
models and agent setups apart. Phase 5 has to measure that: the same coding
tasks under several configurations, repeated, with every run priced by the
gateway. The plan called for about 20 tasks, at least three configurations
and three repeats.

## Decisions

**Pitwall's own agent, not an off-the-shelf one.** The benchmark varies the
harness (how much history is kept, whether caching is on) as well as the
model, so every one of those settings has to be under our control and
nothing else may vary. The agent in `agent/` has five tools (list, read,
write and edit files; run the tests), no shell, retries off, and sends the
task and configuration labels on every call. The cost is realism: agents
with planning or their own context management would spend differently.

**Small Go tasks with the fixes kept out of reach.** Twenty single-package
repositories, standard library only, each with failing tests and a
`TASK.md`. They run in seconds and need nothing installed beyond Go. The
reference fixes live in `bench/solutions/`, outside the directory the agent
works in, and `bench/check_tasks.py` (in CI) checks that each task fails as
shipped and passes with its fix. Several tasks need more than one change,
two span two files, and the tests are table-driven so a partial fix still
fails.

**The runner decides the outcome.** The agent reports what its own test run
said, but an agent can make tests pass by editing them. After each run the
runner restores the original test files, deletes new ones, runs the tests
itself and reports that outcome to the gateway, which overwrites the
agent's. Runs that touched the tests are counted.

**Cost per solved task is total spend over tasks solved.** Failed and
stopped runs count against a configuration, because someone paid for them.
Its interval is a bootstrap over tasks (each resampled task brings all its
repeats), since runs of the same task are not independent. Solve rates get a
Wilson interval. A paired table compares configurations on the same task
and repeat, which removes task difficulty from the comparison and is the
most sensitive view with only three repeats.

**One key per configuration, one team budget per plan.** Keys let the cache
optimizer be on for one configuration and off for another. Putting every key
in a new team with the plan's `budget_usd` as its monthly budget makes the
cap something the gateway enforces with its reserve-then-settle budgets,
not something the runner hopes to respect: at the cap, calls fail with 402
and the runner stops. Runs are ordered repeat by repeat and task by task,
with every configuration on a task before the next, so a stop leaves the
configurations evenly covered. `runs.csv` doubles as the resume log.

**A scripted dry run on the mock.** The mock can't write code, so it got a
`[[mock:script=...]]` directive that plays a fixed list of tool calls. The
runner builds the scripts from the reference fixes: on each task and repeat
the mock solves (after a first change that doesn't help), loops, rewrites
the tests, or gives up, and the choice depends only on the task and repeat,
so every configuration gets the same scripts. That checks every path (solve,
fail, guard stop, cheating caught) and gives paired cost numbers without
spending money. Solve rates from it mean nothing and the report says so.

**Batch trimming, added because of the dry run.** Trimming all but the newest
tool results each turn rewrites an older message on every call, which
invalidates the previous call's cache entry; in the dry run it cost 51% more
than keeping the full history. The agent's `-trim-batch N` moves the trim
point N results at a time so the history is stable in between, and the real
plan runs both variants.

**Standard library only.** The runner and analysis are plain Python 3 with
no packages, and the charts are hand-written SVG, so CI and a fresh clone
can run them with nothing to install.

## Consequences

- Real-model numbers need a provider key in the gateway's environment and a
  budget decision; the dry run is the default and runs in CI.
- The tasks are small. They show per-task overheads (turns, resent context,
  cache writes) clearly but under-represent long sessions, where trimming
  and caching matter most. In the real run (Claude Haiku, Sonnet and Opus
  5.5, $4.98 for 360 runs) every configuration solved every task in 3 to 5
  calls, so the tasks separate the models on cost but not on ability, and
  runs were too short for batch trimming to trim. A harder tier is the
  next step.
- Editing conversation history is not free with signed thinking blocks: the
  real run's per-turn trimming made the API refuse the next call in 27 of 60
  runs. The agent's trimming needs to drop the thinking blocks after the
  edit, or use the API's option to drop mismatched blocks.
- The dry run found two cases where the runaway guard's "same step, same
  result" signal misses a loop (trimmed history, nondeterministic test
  output); the "same tool call" signal still stops them, later. Documented
  in `bench/results/findings.md`.
