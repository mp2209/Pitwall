# Pitwall

An agent-aware AI gateway. Pitwall sits under AI coding agents, groups every
model call into the task it belongs to, and answers the question a
per-request bill can't: **what does it cost to finish a task, and which model
and harness setup does it cheapest?**

> **Status: Phases 0–1 of 6 complete.** The core proxy works: both API
> formats, streaming, tool calls, cross-format translation, and per-call
> token capture. Keys and budgets, task traces, automatic prompt caching,
> the runaway-agent guard and the harness benchmark are next (see
> [Roadmap](#roadmap)). Findings will lead this README once the benchmark
> has results.

## What works today

- **One endpoint, two formats.** `POST /v1/chat/completions` (OpenAI) and
  `POST /v1/messages` (Anthropic), streaming and not, so agents connect by
  changing a base URL.
- **Any format to any provider.** OpenAI, Anthropic and Ollama upstreams.
  Same-format calls are forwarded as raw JSON so no field is lost;
  cross-format calls are translated, including tool calls and their streamed
  argument deltas ([design note](docs/decisions/0001-format-translation.md)).
- **Token capture on every call**, including cache reads and writes, from
  responses and streams. When a provider sends no usage, or the agent hangs
  up mid-stream, the gateway estimates and flags the record.
- **Client disconnects cancel the provider call**, and the tokens used so far
  are still recorded.
- **A mock provider** that speaks both formats, streams text and tool calls,
  injects delays and errors, simulates prompt caching, and has a `loop` mode
  for seeding runaway agents.
- **Checked against real clients**: the official OpenAI and Anthropic Python
  SDKs (in CI) and Claude Code, on both same-format and cross-format routes.

## Quickstart

Run locally with Go 1.27+:

```bash
make build
bin/mockprovider &        # fake provider on :9000
bin/gateway               # gateway on :8080, config/pitwall.yaml
```

Or the full stack (gateway, mock, Postgres, Redis, Ollama, Langfuse):

```bash
cp .env.example .env      # add provider keys if you have them
docker compose up -d --build
```

Try it with the free mock models:

```bash
curl -s localhost:8080/v1/chat/completions \
  -d '{"model":"mock-claude","messages":[{"role":"user","content":"hello"}]}'
```

`mock-claude` is served by the Anthropic-format mock, so that call is an
OpenAI-format request translated to Anthropic and back.

### Point an agent at it

**Claude Code** (Anthropic format):

```bash
ANTHROPIC_BASE_URL=http://localhost:8080 claude
```

With `ANTHROPIC_API_KEY` set in the gateway's environment, `claude-*` models
go to Anthropic. To run Claude Code on a local model instead, set
`ANTHROPIC_MODEL=ollama/qwen2.5-coder:7b` (and the same for
`ANTHROPIC_DEFAULT_HAIKU_MODEL`); the gateway translates to Ollama's OpenAI
format.

**OpenAI SDKs and OpenAI-format agents:** set the base URL to
`http://localhost:8080/v1`.

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

Provider keys come from the environment variables named by `api_key_env`
and never leave the gateway. Until Phase 2 adds virtual keys, `dev_keys` is a
plain allow-list (empty means no auth, for local use only).

## Development

```bash
make test       # go test -race ./...
make lint       # golangci-lint
make smoke      # official SDKs against a running gateway + mock
```

## Layout

```
cmd/gateway/          gateway binary
cmd/mockprovider/     mock provider binary
internal/api/         HTTP handlers, streaming relay, call records
internal/providers/   routing, upstream requests, wire types per format
internal/translate/   OpenAI <-> Anthropic requests, responses, streams
internal/sse/         server-sent event reader and writer
internal/metering/    normalized token usage
internal/config/      YAML config
mockprovider/         fake OpenAI + Anthropic server
deploy/               docker-compose.yml, Postgres init
docs/decisions/       design notes
scripts/              SDK smoke test
```

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Repo, CI, Compose stack, mock provider | done |
| 1 | Both API formats, streaming, tool calls, OpenAI/Anthropic/Ollama adapters | done |
| 2 | Postgres schema, virtual keys, cost from a price table, async request log, reserve-then-settle budgets in Redis | next |
| 3 | Task traces: `x-pitwall-task` header, inferred tasks, Langfuse, Grafana task panel | |
| 4 | Automatic prompt-cache breakpoints (switchable per key); runaway-agent guard | |
| 5 | Harness benchmark: ~20 coding tasks × configurations × 3 repeats | |
| 6 | Write-up: findings, blog post, demo | |
