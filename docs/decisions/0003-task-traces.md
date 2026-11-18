# 0003: Task traces and inference

## Context

Pitwall's unit of measurement is the agent task: one run, such as "make this
failing test pass", from its first model call to its last. Phase 3 has to
group calls into tasks, record per-task cost, call count and context growth,
and do it for agents that can be changed (our own agent loop, the benchmark
runner) and agents that can't.

## Decisions

**Two ways in, header first.** A call that carries `x-pitwall-task` belongs to
that task. The task ID is derived from the key and the label
(`t_` + SHA-256 of both), so every gateway replica agrees with no shared state
and two keys using the same label never share a task. Calls without the
header are grouped by inference (`ti_` + random ID). `x-pitwall-agent` and
`x-pitwall-config` label the harness and the configuration under test; the
agent name falls back to the User-Agent product (`claude-cli`, `openai`). The
response carries `x-pitwall-task-id`, so a client can report an outcome for an
inferred task too.

**Inference follows how agents work: every call resends the conversation.**
Each request is reduced to pieces in the order providers read them (tools,
system prompt, then one piece per message), with a cumulative hash after each
piece. Two calls share their first k pieces exactly when their first k hashes
match. `cache_control` markers are removed before hashing, because clients
move them to the newest turn on every call and an unchanged message would
otherwise look edited.

Candidate tasks are the open conversations of the same key with the same
opening (system prompt and first user message), seen within the idle gap
(default 5 minutes). For each call:

1. If it extends a candidate's previous call (it starts with every piece of
   that call), it joins that task; the longest match wins, so two runs of the
   same task stay apart once their histories differ.
2. Otherwise, if the history was edited rather than extended (a harness
   trimming old tool output, a moved reminder), it joins the most recent
   candidate.
3. Otherwise it starts a new task.

A call shaped like a fresh start (one or two messages) never continues a
candidate whose last call had more messages, or whose last call it repeats
exactly: after an answer, an agent's next call grows the history, so an
identical opening call is a new run (a benchmark repeat, a second Claude Code
session with the same prompt). The cost is that a retry of a failed first call
starts a task of its own worth $0, which is easy to filter; merging two runs
would corrupt cost per task. When two candidates match equally, the most
recent wins.

Open conversations live in Redis (one key per key and opening, at most 8,
with the idle gap as TTL), updated with WATCH/MULTI and jittered retries, so
replicas infer the same tasks. Without Redis an in-memory store is used.

**Inference runs even when the header is present.** Every request row stores
`inferred_task_id` alongside `task_id`, so the benchmark can score inference
against the header: how often the two partitions of calls agree. The plan
treats that accuracy as a result in itself.

**Repeated input is measured, not guessed.** The shared prefix with the task's
previous call gives the bytes resent unchanged; `repeated_input_tokens` is the
call's prompt tokens scaled by that byte share. It is an estimate of how much
of each prompt is repeated context, the share prompt caching can recover.

**Task totals are folded in with the request rows.** The async writer copies a
batch of request rows and updates their tasks' totals (calls, tokens, cost,
largest context, repeated input, time span) in the same transaction, in a
fixed task order so replicas can't deadlock. A task row therefore always
agrees with its calls. An outcome (`POST /v1/pitwall/tasks/outcome`) may
arrive before the last calls are written; it creates a stub row that the
calls fill in without overwriting the outcome. Only the key that made a task
can set its outcome.

**Per-task numbers live in Postgres, not Prometheus.** Task IDs as labels
would create a time series per task. Prometheus gets gateway-wide counters
and histograms (cost and tokens by model, cost without caching, gateway
overhead, time to first token, tasks started, budget refusals, guard
triggers), with the model label limited to priced models so clients can't
mint series. Grafana reads per-task panels straight from Postgres.

**Traces carry the turn, not the whole prompt.** Langfuse gets one trace per
task and one generation per call. Every call upserts its task's trace with the
same fields, including the task's start time kept in the tracker state, so the
trace exists whichever call or gateway replica Langfuse hears from first (a
task's first recorded call is not always its first call, for example when an
agent drops the header midway). A generation's input is only the messages
the call added since the task's previous call, so a trace reads as the run's
turns rather than a 100k-token prompt resent N times. Inputs, outputs and
error messages are redacted (credentials, emails, phone numbers, card numbers
that pass the Luhn check) and capped at 32 KB per field before they leave.

## Known limits

- Two runs with identical openings, started at the same moment under one key
  without headers, merge until their histories diverge; they may stay merged.
  The benchmark always sends headers.
- Claude Code's side calls (titles, summaries) have their own system prompts,
  so they appear as separate one-call tasks.
- History rewrites that shrink the conversation to one or two messages (a
  full compaction) start a new inferred task.
