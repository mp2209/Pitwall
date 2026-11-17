# 0002: Virtual keys, metering and budgets

## Context

Phase 2's goal is a correct cost row for every call, and a monthly budget per
key. The plan keeps this deliberately small (it overlaps most with existing
gateways) so the time goes to the agent features. The decisions below are
the ones that affect correctness.

## Decisions

**Keys are hashed with SHA-256, not a password hash.** Keys are `pw-` plus
192 random bits, so they can't be guessed and a fast hash is enough; bcrypt
or argon2 would add milliseconds to every request. The first 11 characters
are stored in plain text to tell keys apart. Lookups are cached for 30
seconds (unknown keys for 5), so a revoked or edited key takes effect at
once on the replica that changed it and within 30 seconds on the others.

**Money is integer nanodollars.** One cached token on the cheapest models
costs about $0.00000001, which micro-dollars would round to zero. Integers
keep Redis counters exact (`HINCRBY`) and sum exactly in Postgres
(`numeric(18,9)`). Lua numbers are doubles, exact up to about $9M per counter.

**Prices are rows with an effective date, never edited.** `config/prices.yaml`
is the reviewed source. At startup its rows are inserted into `model_prices`
if missing; existing rows are never changed. A price change is a new row
with a later `effective_from` (in the file or via `POST /admin/prices`), so a
call is always priced at the rate in effect when it started. The price model
covers what the providers actually charge for: separate 5-minute and 1-hour
cache-write rates, long-prompt tiers (Haiku 5.5 over 100k tokens counting
cache reads and writes, OpenAI flagships over 272k), separate fast-mode rows,
and Anthropic's 1.1x for US-only inference. Lookups try the exact model name,
then the name without a snapshot date, then the longest `prefix*` row. A call
with no matching price is recorded at $0 with `priced = false` and logged
once per model, rather than guessed. Ollama is marked `free`.

**Reserve the worst case, settle the real cost.** Before sending, the gateway
reserves the most the call can cost: the prompt (estimated at three bytes per
token, leaning high) at the dearest input rate (a 1-hour cache write), plus
`max_tokens` of output, times any US multiplier. One Lua script checks every
scope the call counts against (key and team) and reserves on all of them or
none. After the call, a second script releases the hold and adds the real
cost. Concurrent calls therefore cannot overrun a limit together. The cost
of this safety is that a key close to its limit starts refusing calls whose
worst case doesn't fit, even if their real cost would have; lowering
`max_tokens` gets such a call through.

**402, not 429, for an exhausted budget.** The OpenAI and Anthropic SDKs and
Claude Code retry 429 with backoff, which would turn a budget stop into a
retry loop. 402 is not retried; it arrives as `insufficient_quota` (OpenAI
format) or `billing_error` (Anthropic format) with a message naming the key,
the spend, the limit and the call's worst case.

**Redis counters are a cache of Postgres.** Counters are one hash per scope
and month (`pitwall:{budget}:2026-11:key:12`, with a Redis Cluster hash tag
so the multi-key scripts work). If a counter is missing (first call of the
month, or Redis lost its data), it is seeded from the sum of the month's
cost rows before the reservation proceeds. `HSETNX` keeps the first value if
two replicas seed at once.

**Fail open by default when Redis is down.** A budget outage shouldn't stop
every developer, so by default calls go through, the failure is logged, and
the spend still lands in Postgres (and in Redis once it is back, via
seeding). `budget_fail_open: false` refuses calls instead.

**The request log is written off the request path.** Rows go into a bounded
in-process queue and are written in batches with `COPY` every 250 ms or 500
rows. If the queue is full (the database is down or slow), rows are dropped
and counted rather than slowing an agent; shutdown drains the queue. The
budget counter is updated on the request path, so budgets stay correct even
when rows are dropped.

**Estimated usage uses o200k_base.** When a provider reports no usage, or a
stream is cut off, tokens are counted with OpenAI's current tokenizer, which
is exact for OpenAI models and an approximation for Claude (Anthropic
publishes no local tokenizer). Such rows are flagged `estimated`.

## Not covered yet

- OpenAI service tiers (Flex, Batch, Fast) and its regional pricing; calls
  are priced at Standard rates.
- Per-use charges for server tools (web search, code execution).
- Requests rejected before a key is known (bad JSON, unknown model) are not
  recorded; budget refusals are, at $0.
