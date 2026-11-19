# Pitwall

An agent-aware AI gateway. Pitwall sits under AI coding agents, groups every
model call into the task it belongs to, and answers the question a
per-request bill can't: **what does it cost to finish a task, and which model
and harness setup does it cheapest?**

> **Status: Phases 0–3 of 6 complete.** The proxy speaks both API formats,
> every call carries a virtual key, a cost row and a budget check, and calls
> are grouped into agent tasks with per-task cost, call count and context
> growth. Automatic prompt caching, the runaway-agent guard and the harness
> benchmark are next (see [Roadmap](#roadmap)). Findings will lead this
> README once the benchmark has results.

## What works today

**Proxy**

- **One endpoint, two formats.** `POST /v1/chat/completions` (OpenAI) and
  `POST /v1/messages` (Anthropic), streaming and not, so agents connect by
  changing a base URL.
- **Any format to any provider.** OpenAI, Anthropic and Ollama upstreams.
  Same-format calls are forwarded as raw JSON so no field is lost;
  cross-format calls are translated, including tool calls and their streamed
  argument deltas ([design note](docs/decisions/0001-format-translation.md)).
- **Client disconnects cancel the provider call**, and the tokens used so far
  are still recorded and charged.

**Keys, cost and budgets** ([design note](docs/decisions/0002-keys-metering-budgets.md))

- **Virtual keys.** One `pw-` key per developer, stored only as a hash. Each
  key belongs to a user and team, can be limited to model patterns, and has
  a monthly budget. Real provider keys never leave the gateway.
- **A cost row for every call** in Postgres: tokens by kind (input, cache
  reads, 5-minute and 1-hour cache writes, output), cost, latency, time to
  first token and gateway overhead. Rows are written in batches off the
  request path.
- **Prices from a dated table** ([`config/prices.yaml`](config/prices.yaml),
  read from Anthropic's and OpenAI's pricing pages), including long-prompt
  tiers, fast mode and Anthropic's US-only multiplier. A price change is a
  new row, so recorded costs never change.
- **Budgets that can't be overrun by concurrency.** Each call reserves its
  worst-case cost in Redis before it is sent and settles to the real cost
  after; ten calls arriving at once for a key with $1 left are admitted only
  while their reservations fit. Key and team limits are checked together.
  An exhausted budget returns `402` with a message the agent shows its user.
- **Usage when the provider sends none** is counted with a tokenizer and
  flagged as estimated.

**Task traces** ([design note](docs/decisions/0003-task-traces.md))

- **Every call joins a task.** Clients that can send `x-pitwall-task` name
  their run; for agents that can't, the gateway infers the task from the
  conversation each call resends (same system prompt and opening turn,
  history extended, closed after an idle gap). Inference also runs alongside
  the header, so its accuracy can be measured.
- **Per-task numbers in Postgres**: calls, cost, tokens, wall time, the prompt
  size of every turn, the share of input repeated from the previous call, and
  the outcome the agent reports (`solved`, `failed`, `stopped`).
- **Dashboards and traces**: a Grafana dashboard lists tasks with cost, call
  count and context growth, plus cost per solved task by configuration and
  gateway metrics from Prometheus. Langfuse gets one trace per task and one
  generation per call, redacted (credentials, emails, phone and card numbers).
- **A minimal agent** ([`agent/`](agent/agent.go)) for the benchmark: read
  files, run the tests, edit, repeat; no shell tool; optional trimming of old
  tool output; it reports its outcome from its own test run.

**Tested against real clients**: the official OpenAI and Anthropic Python
SDKs (in CI) and Claude Code, on both same-format and cross-format routes,
with a virtual key. In the Phase 2 end-to-end run, 18 calls produced 18
priced cost rows, each one recomputing exactly from its token counts, and the
live budget counter matched the database total.

## Quickstart

### Full stack (recommended)

Gateway, mock provider, Postgres, Redis, Ollama and Langfuse:

```bash
cp .env.example .env      # add provider keys and an admin token
docker compose up -d --build
```

Create a team, a developer and their key with the admin token
(`pw-admin-dev` unless you set `PITWALL_ADMIN_TOKEN` in `.env`):

```bash
ADMIN="Authorization: Bearer pw-admin-dev"
curl -s localhost:8080/admin/teams -H "$ADMIN" -d '{"name":"core","monthly_budget_usd":100}'
curl -s localhost:8080/admin/users -H "$ADMIN" -d '{"email":"dev@example.com","team_id":1}'
curl -s localhost:8080/admin/keys  -H "$ADMIN" -d '{"user_id":1,"name":"laptop","monthly_budget_usd":25}'
```

The last call prints the key once; Pitwall keeps only its hash. Then make a
call with it, using the free mock models:

```bash
curl -s localhost:8080/v1/messages -H "x-api-key: $PITWALL_KEY" \
  -d '{"model":"mock-claude","max_tokens":200,"messages":[{"role":"user","content":"hello"}]}'
```

Non-streaming responses carry the call's cost in `x-pitwall-cost-usd` and
every response its task in `x-pitwall-task-id`. Call rows are at
`GET /admin/requests`, tasks at `GET /admin/tasks`.

Dashboards: Grafana at http://localhost:3001 (the Pitwall dashboard opens by
default), Langfuse at http://localhost:3000 (dev login `admin@pitwall.local`,
password in `deploy/docker-compose.yml`), Prometheus at http://localhost:9091.

### Dev mode

Without `PITWALL_DATABASE_URL` the gateway runs with no keys or cost rows
(calls are only logged), which is handy for working on the proxy itself:

```bash
make build
bin/mockprovider &        # fake provider on :9000
bin/gateway               # gateway on :8080, config/pitwall.yaml
```

### Point an agent at it

**Claude Code** (Anthropic format):

```bash
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=$PITWALL_KEY claude
```

With `ANTHROPIC_API_KEY` set in the gateway's own environment, `claude-*`
models go to Anthropic. To run Claude Code on a local model instead, set
`ANTHROPIC_MODEL=ollama/qwen2.5-coder:7b` (and the same for
`ANTHROPIC_DEFAULT_HAIKU_MODEL`); the gateway translates to Ollama's OpenAI
format and records the calls at $0.

**OpenAI SDKs and OpenAI-format agents:** base URL
`http://localhost:8080/v1`, API key `$PITWALL_KEY`.

### Tasks

Label a run so its calls form one task, and report how it ended:

```bash
curl -s localhost:8080/v1/messages -H "x-api-key: $PITWALL_KEY" \
  -H "x-pitwall-task: fix-parser-1" -H "x-pitwall-config: sonnet/full-history" \
  -d '{"model":"mock-claude","max_tokens":200,"messages":[{"role":"user","content":"Fix the parser"}]}'
curl -s localhost:8080/v1/pitwall/tasks/outcome -H "Authorization: Bearer $PITWALL_KEY" \
  -d '{"task":"fix-parser-1","outcome":"solved"}'
```

Claude Code can send the label with
`ANTHROPIC_CUSTOM_HEADERS="x-pitwall-task: my-task"`; without it, its calls
are grouped by inference.

### The agent

Run Pitwall's own agent on a task directory (it edits files in place, so use
a copy):

```bash
cp -R bench/tasks/001-word-count /tmp/wc
PITWALL_KEY=... bin/agent -model claude-sonnet-5-5 -dir /tmp/wc -config sonnet/full
```

It sends `x-pitwall-task`, runs the task's tests when it stops, reports the
outcome, and prints a JSON summary. `-keep-tool-results N` trims all but the
newest N tool outputs, the "old tool output trimmed" context strategy the
benchmark compares.

## Admin API

Authenticate with `Authorization: Bearer $PITWALL_ADMIN_TOKEN`, or with the
virtual key of a user whose role is `admin`. Budgets are monthly, in USD,
reset at the start of each UTC month; `0` means no limit.

| Method and path | Does |
|---|---|
| `POST /admin/teams` | `{name, monthly_budget_usd}` |
| `GET /admin/teams` | Teams with month-to-date spend |
| `PATCH /admin/teams/{id}` | `{monthly_budget_usd}` |
| `POST /admin/users` | `{email, team_id, role: "dev" \| "admin"}` |
| `GET /admin/users` | Users |
| `POST /admin/keys` | `{user_id, name, allowed_models, monthly_budget_usd, cache_optimizer_on}`; returns the key once |
| `GET /admin/keys` | Keys (prefix only) with spend and amount held for calls in flight |
| `PATCH /admin/keys/{id}` | Any of `name`, `allowed_models`, `monthly_budget_usd`, `cache_optimizer_on` |
| `DELETE /admin/keys/{id}` | Revoke |
| `GET /admin/prices` | The price table in effect |
| `POST /admin/prices` | Add a price row; `effective_from` defaults to now |
| `GET /admin/requests?key_id=&limit=` | Newest cost rows |
| `GET /admin/tasks?key_id=&config_label=&outcome=&source=&limit=` | Tasks with totals, context growth and repeated-input share |
| `GET /admin/tasks/{id}` | A task and its calls in order |

Agents report outcomes with their own key: `POST /v1/pitwall/tasks/outcome`
with `{"task": "<x-pitwall-task label>"}` or `{"task_id": "<x-pitwall-task-id>"}`
and `"outcome": "solved" | "failed" | "stopped"`.

## Configuration

[`config/pitwall.yaml`](config/pitwall.yaml) lists providers and routing
rules. Rules are tried in order and the first match wins:

```yaml
models:
  - match: "claude-*"          # path.Match glob
    provider: anthropic
  - match: "ollama/*"
    provider: ollama
    strip_prefix: "ollama/"    # ollama/qwen2.5-coder:7b -> qwen2.5-coder:7b
  - match: cheap
    provider: ollama
    upstream: "qwen2.5-coder:7b"
```

Environment:

| Variable | Effect |
|---|---|
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` | Real provider keys (named by `api_key_env`) |
| `PITWALL_DATABASE_URL` | Turns on virtual keys, cost rows and the admin API |
| `PITWALL_REDIS_URL` | Turns on budgets (needs the database too) |
| `PITWALL_ADMIN_TOKEN` | Bootstrap bearer token for `/admin` |
| `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY` | Turn on Langfuse traces |

`budget_fail_open` (default `true`) decides what happens when Redis is down:
let calls through and log it, or refuse them. `task_idle_gap` (default `5m`)
closes inferred tasks; `metrics_listen` (default `:9464`) serves Prometheus
metrics on their own port.

## Development

```bash
make test       # go test -race ./...
make lint       # golangci-lint
make smoke      # official SDKs against a running gateway + mock
```

Store, budget and full-stack tests need Postgres and Redis and are skipped
unless these are set (CI sets them):

```bash
export PITWALL_TEST_DATABASE_URL="postgres://pitwall:pitwall@localhost:5432/pitwall?sslmode=disable"
export PITWALL_TEST_REDIS_URL="redis://localhost:6379/0"
```

Each test gets its own Postgres schema, and the Redis test uses random keys
and never flushes.

## Layout

```
cmd/gateway/          gateway binary
cmd/mockprovider/     mock provider binary
cmd/agent/            agent binary
agent/                minimal agent loop used by the benchmark
internal/api/         HTTP handlers, streaming relay, billing, admin and task API
internal/tasks/       task header and inference
internal/telemetry/   Prometheus metrics, Langfuse exporter, PII redaction
internal/auth/        virtual keys, hashing, key cache
internal/metering/    usage, prices and cost, Redis budgets, tokenizer
internal/store/       Postgres migrations, queries, async request writer
internal/providers/   routing, upstream requests, wire types per format
internal/translate/   OpenAI <-> Anthropic requests, responses, streams
internal/sse/         server-sent event reader and writer
internal/config/      YAML config and price file
mockprovider/         fake OpenAI + Anthropic server
bench/tasks/          benchmark tasks: tiny repos with failing tests
config/               gateway config and price table
deploy/               docker-compose.yml, Postgres init, Prometheus, Grafana
docs/decisions/       design notes
scripts/              SDK smoke test
```

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Repo, CI, Compose stack, mock provider | done |
| 1 | Both API formats, streaming, tool calls, OpenAI/Anthropic/Ollama adapters | done |
| 2 | Postgres schema, virtual keys, cost from a price table, async request log, reserve-then-settle budgets in Redis | done |
| 3 | Task traces: `x-pitwall-task` header, inferred tasks, Langfuse, Grafana task panel | done |
| 4 | Automatic prompt-cache breakpoints (switchable per key); runaway-agent guard | next |
| 5 | Harness benchmark: ~20 coding tasks × configurations × 3 repeats | |
| 6 | Write-up: findings, blog post, demo | |
