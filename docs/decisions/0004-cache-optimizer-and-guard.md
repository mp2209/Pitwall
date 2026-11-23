# 0004: Cache optimizer and runaway-agent guard

## Cache optimizer

### Context

Agents resend their tools, system prompt and conversation on every turn.
Anthropic caches a prompt prefix only where the request marks it with
`cache_control`; a cache read costs 0.025x to 0.1x of plain input and a
5-minute write 1.25x. Many clients never add the marks. OpenAI caches long
prefixes automatically, so there the gateway only measures.

### Decisions

**Two breakpoints, placed the way Anthropic's guidance recommends for agent
loops.** One on the last system block (or the last tool when there is no
system prompt), which caches tools and system together and survives any
change later in the conversation. One on the last eligible block of the
newest turn, so the next call reads the whole conversation so far and only
writes what it added. Each marker looks back up to 20 positions for an
earlier entry, so moving the tail marker forward each turn still hits the
previous turn's entry.

**Hands off when the client manages caching.** A request that already carries
any `cache_control` (Claude Code's do) is forwarded unchanged; mixing our
markers with the client's could exceed the 4-marker limit or fight the
client's placement.

**Respect the minimum.** Prefixes shorter than the model's minimum (512 tokens
on the 5.x models, up to 4,096 on Opus 4.5/4.6 and Haiku 4.5) are never
cached, so no marker is placed there. Only text, image, tool_use,
tool_result and document blocks can carry a marker; thinking blocks and
empty text are skipped, and so is a trailing mid-conversation system message.

**5-minute TTL.** Agent turns start well under five minutes apart, which keeps
the 5-minute entry warm; the 1-hour TTL doubles the write price for nothing
here.

**Switched per key, measured both ways.** `api_keys.cache_optimizer_on` turns
it on, so the same tasks can run with it off and on. Every call records the
breakpoints it added and the optimizer's outcome (`applied`,
`client_managed`, `too_short`, `off`).

### When caching costs more

A marked prefix that is never read again costs 1.25x instead of 1x. That
happens on a task's last call (its tail is written for a next turn that never
comes), on one-call tasks, and when a harness edits history before the
cached point: trimming old tool output rewrites messages the previous turn
cached, so the next call misses everything after the edit and writes it
again. The optimizer doesn't try to predict these; the experiment measures
them.

## Runaway-agent guard

### Context

A stuck agent repeats the same failing step and burns budget. The guard has
to tell that apart from an agent legitimately retrying.

### Decisions

**Two loop signals, read from the conversation itself.** Every call resends
the history, so repetition at its end can be measured from the request alone,
with no stored state, for labeled and inferred tasks alike:

- `repeated_exchange`: the same assistant step followed by the same result,
  N times in a row. This is the stuck agent. Warn at 3, stop at 5.
- `repeated_tool_call`: the same tool call with the same arguments, N times
  in a row, whatever came back. Polling looks like this, so the limits are
  higher: warn at 6, stop at 10.

Comparisons ignore what always differs between attempts and says nothing
about progress: tool-use IDs, thinking blocks, argument key order, digits
(test output carries timings, so a run that fails the same way must look the
same), and the order of lines in a tool result. The last one came from the
first measurement run: Go printed the example task's failures in a different
order on every run (its test iterates over a map), so identical failures
rarely matched, and most seeded loops were only caught by the looser
repeated-tool-call limit at call 11 instead of the repeated-exchange limit at
call 6. Six runs of that test gave three distinct outputs, all the same once
lines were sorted. Parallel test runners reorder output the same way.

**Stuck versus retrying.** A retry that changes something breaks the run: an
edit between two test runs is a different exchange in between, and a
different result is a different exchange. Only repetition with nothing
changing counts toward the low limits. The cost: a counter in otherwise
identical output ("attempt 2", "attempt 3") is treated as no change.

**Two more signals from the plan.** `context_size` (estimated prompt tokens;
warn at 200k, stop at 500k) and `task_cost` (the task's cost so far, kept in
Redis per task and added after each call; warn at $8, stop at $10). The cost
cap is checked before a call, so a task can pass it by at most one call.

**Warn, then stop.** A warning adds `x-pitwall-warning` and lets the call
through. A stop refuses it with a 400, which SDKs and agents don't retry
(429, 409 and 5xx are retried), and a message naming the reason and the
repeated call, which the agent shows its user. The guard runs before the
budget reservation, so a refused call holds no money. The task is marked
`stopped` unless the agent reports another outcome.

**No sticky verdict.** The guard re-reads the conversation on every call, so
an agent that changes approach after a warning or a stop drops below the
limits and can carry on.

**Limits are configuration.** The `guard` section of the config sets every
limit; 0 turns a step off.
