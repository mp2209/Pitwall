# 0001: Format translation and passthrough

## Context

Pitwall accepts two client formats (OpenAI Chat Completions, Anthropic
Messages) and calls three providers (OpenAI, Anthropic, Ollama). Agents only
let you change the base URL of the API they already speak, so both formats
must work against every provider.

## Decisions

**One adapter per wire format, not per vendor.** Ollama serves the OpenAI
format, so it is an OpenAI-kind provider with a different base URL and no
key. That leaves two formats and four client/provider pairs.

**Same format: forward the raw body.** The handler decodes only what it
needs (model, stream flag), then patches the raw JSON object (model name,
`stream_options`) and forwards it. Fields Pitwall does not model, such as
Anthropic `thinking` settings, `cache_control`, beta features or OpenAI
`response_format`, reach the provider untouched. Same-format streams are
relayed byte for byte while a tap reads usage from the events.

**Cross format: translate through typed structs.** Some loss is unavoidable,
and each conversion's doc comment lists it. The parts that need care:

- *Tool results.* OpenAI sends one `tool` message per result; Anthropic wants
  all results for a turn as `tool_result` blocks in one user message, with
  roles alternating. Going to Anthropic, consecutive same-role turns are
  merged. Going to OpenAI, tool results are emitted as `tool` messages
  before the rest of the user turn, because OpenAI requires them directly
  after the assistant's tool calls.
- *Tool arguments.* OpenAI carries them as a JSON string, Anthropic as an
  object. Arguments that are not a JSON object are kept under an
  `arguments` key rather than dropped.
- *Streams.* Anthropic streams typed content blocks; OpenAI streams deltas
  with a per-tool-call index. The translators are small state machines: an
  Anthropic `tool_use` block becomes a new OpenAI tool index, and a run of
  OpenAI content or tool deltas becomes one Anthropic block, with
  `content_block_stop` before the next block starts.
- *System turns.* OpenAI system and developer messages become the
  Anthropic system prompt. Claude Code also sends `system`-role turns inside
  `messages`; going to OpenAI these stay system messages in place.
- *max_tokens.* Anthropic requires it, so OpenAI requests without one get
  `default_max_tokens`. Going to OpenAI, the field name is set per provider
  (`max_completion_tokens` for OpenAI's newer models, `max_tokens` for
  Ollama).

**Always ask OpenAI-format providers for usage.** The gateway sets
`stream_options.include_usage` on every streamed OpenAI-format call so the
final chunk carries token counts. If the client did not ask for that chunk,
the passthrough drops it again, since some clients assume every chunk has a
choice.

**Estimate when usage is missing, and say so.** If a provider reports no
usage, or the client disconnects before the final event, the gateway fills
the gap at four bytes per token and sets `Estimated` on the record. Input
counts that did arrive (Anthropic's `message_start`) are kept as reported.
Phase 2 replaces the byte heuristic with a tokenizer.

## Consequences

- Same-format calls cost one JSON decode and re-encode of the top-level
  object; nested content is copied as raw bytes.
- Cross-format calls drop thinking blocks, Anthropic server tools (web
  search, code execution), `top_k`, and OpenAI `n`, `logprobs` and
  `response_format`.
- Requests rejected before reaching a provider (bad JSON, unknown model) are
  not recorded as model calls yet.
