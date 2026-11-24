# Phase 5 findings

Two runs of the harness benchmark:

- **Real models** ([results](claude/results.md), [plan](../plans/claude.json)):
  360 runs, 20 tasks x 6 configurations x 3 repeats, Claude Haiku 5.5,
  Sonnet 5.5 and Opus 5.5 through Pitwall on 2026-11-24. Total spend
  $4.98 against a $40 cap.
- **Dry run on the mock** ([results](dry-run/results.md)): 200 runs, 20
  tasks x 5 configurations x 2 repeats, with scripted outcomes (see the
  [benchmark README](../README.md#dry-run-on-the-mock)). Token counts,
  costs, cache use, guard stops and task grouping are what the gateway
  measured; solve rates are set by the script.

## Real models

### 1. Every configuration solved every task

360 of 360 runs solved, verified with the original tests restored (95%
interval for each configuration: 94% to 100%). No model edited the tests,
and every agent's own report matched the runner's check. Runs took 3 to 5
model calls. The semver comparison and the retry backoff took the most
calls; on Sonnet, the duration parser and the semver comparison cost the
most.

So on these tasks the models don't differ in what they can do, only in what
it costs. The tasks are a fair test of per-task overhead, but separating the
models on ability needs harder ones: larger repositories, several packages,
vaguer task statements, runs long enough that history management matters.

### 2. The cheapest model that can do the job wins by an order of magnitude

| Configuration | Cost per solved task | Against Sonnet with caching (paired) |
|---|---|---|
| Haiku 5.5, caching | $0.0009 | -93%, cheaper on 60 of 60 |
| Sonnet 5.5, caching | $0.0128 | - |
| Sonnet 5.5, no caching | $0.0185 | +45% |
| Opus 5.5, caching | $0.0253 | +98%, cheaper on 0 of 60 |

Haiku took slightly more calls (3.9 per task against Sonnet's 3.6) and more
tokens (8.3k per task against 6.5k), and it was still 14 times cheaper than
Sonnet and 28 times cheaper than Opus, and the fastest (median 4.8s per
task, against 6.5s and 9.1s). When a task is within reach of the small
model, per-task cost is decided by price, not by how efficiently a model
works.

### 3. Caching cut Sonnet's cost by 31%, even on three-call tasks

With full history, the gateway's cache breakpoints cut Sonnet's total cost
31% against the same runs without them (paired by task and repeat; cheaper
on 59 of 60). That is less than the dry run's 53% because the real runs
were shorter, 3.6 calls against 9: each run pays one cache write and gets
only two or three reads. Output is now 56% of a cached Sonnet task's cost,
so caching input can't take much more off tasks this short.

### 4. Trimming old tool output broke the conversation

The agent's trimming replaces older tool results with a short placeholder.
Sonnet 5.5 returned signed thinking blocks with its answers, and a thinking
block is bound to the conversation before it. Once an earlier tool result
changed, the API refused the next call with a 400: the block "is bound to a
different conversation". That happened in 27 of the 60 runs with per-turn
trimming, always on the agent's last call, after the fix was already written, so
those runs still count as solved. On a longer task,
the same error would end the run midway.

Batch trimming had no errors because it never trimmed anything: with three
results kept and a batch of four, the first trim needs seven tool results,
and none of these runs got that far (none of them hit the error, and a
trim would almost certainly have caused it). Neither trimming variant changed the cost at this length (+1.5% and
-2.0% against full history with caching, both well within noise).

The API's error names two ways out: drop the thinking blocks that follow
the trimmed point, or opt in (a beta header) to having the API drop
mismatched blocks itself. An agent or gateway that edits history has to do
one of them; Pitwall's agent doesn't yet.

### 5. The guard stayed quiet, and task grouping was exact

The runaway guard neither warned nor stopped any of the 360 runs. None of
them looped, so this shows the guard leaves normal work alone; its catch
rate comes from the mock (below). Pitwall's task inference put all 360 runs
in exactly one task each, with no splits and no merges. Each configuration
had its own key and inference works per key, so a key rarely had more than
one or two runs in flight; this is not a test of many concurrent tasks on
one key.

## Dry run on the mock

### 6. The pipeline holds together

200 of 200 runs were recorded, priced and grouped into one task each. The
runner caught every run that "passed" by rewriting the tests: 10 of 10
reported solved by the agent, failed with the original tests restored.
Calls over the OpenAI format (`mock-gpt`, translated by the gateway from the
agent's Anthropic requests) behaved the same as native ones.

### 7. Caching halves the cost of a nine-call task

On scripted nine-call runs with full history, cache breakpoints cut cost
53% (paired; cheaper on 35 of 40). The 5 runs it didn't help were short and
never reached the mock's 1,024-token minimum, so nothing was cached.

### 8. Trimming every turn works against caching; trimming in batches doesn't

Trimming all but the newest tool results each turn sent fewer tokens (16.6k
per solved task against 19.2k) but cost **51% more** than full history with
caching, cheaper on only 2 of 40 paired runs. Each trim rewrites an older
message, which invalidates the cache entry the previous call wrote, so only
the tools and system prompt are read back (36% cache read share against
71%), and the rewritten history is paid for again as a cache write.

Moving the trim point in batches of 4 (`-trim-batch 4`, added after this
showed up) leaves the history unchanged for three calls out of four. That
recovered most of the loss: 28% cheaper than trimming every turn (cheaper on
38 of 40), within 8.5% of full history. The real run (finding 4) adds a
second reason to batch, or to avoid editing history at all: every edit also
invalidates the thinking blocks after it.

### 9. Two blind spots in the runaway guard

The guard stopped all 20 seeded loops (4 per configuration), and it neither
stopped nor warned any other run. In 12 of them it refused the 6th call, as
in Phase 4. The other 8 got through to the 11th call, where the separate
"same tool call" signal (stop at 10) caught them:

- **Trimmed history hides the repetition** (all 4 loops with per-turn
  trimming). The "same step, same result" check compares tool results, and
  once older results are replaced by a placeholder, only the untrimmed ones
  still look alike. With 2 results kept, the run of identical exchanges
  never got past 2. Batch trimming happened to dodge this here, because the
  loop reached the stop limit before the first trim.
- **Nondeterministic failure output** (the topological-sort loop, in all 5
  configurations). The buggy sort prints its wrong answer in a different
  order on each run, because it walks a Go map, so the same failing test,
  run again and again, never gave the same result twice.

On the same task, a loop caught at the 11th call cost about 2.4 times one
caught at the 6th ($0.020 against $0.008 on the mock's prices). The fix is
to compare a coarser signature of a result (the names of the failing tests
rather than the whole output) and to treat a result that became a trim
placeholder as unchanged.

## What's next

- A harder task tier, so solve rates differ and "a cheap model that needs
  three tries" can show up in cost per solved task.
- Trimming that keeps the conversation valid with signed thinking blocks,
  then a rerun of the trimming configurations on tasks long enough to trim.
- The guard fix for finding 9.
