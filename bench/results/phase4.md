# Phase 4 measurements

Generated 20261119T055338 UTC by `bench/experiments/phase4.py`, 5 runs per cache configuration.

**Read this first:** these runs use the mock provider, not a real model. The mock simulates
Anthropic's prompt cache (writes at breakpoints, 20-position lookback, 1,024-token minimum) and
plays a scripted agent: 12 tool calls cycling through the agent's tools, or, for seeded loops, the
same failing test run every turn. Costs use the mock's price row ($3 input, $3.75 cache write,
$0.30 cache read, $15 output per million tokens). The numbers measure the gateway's mechanisms;
real-model numbers come from the Phase 5 benchmark.

## Cache optimizer

| Configuration | Runs | Calls per task | Cost per task | Cached share of input | Breakpoints added per task |
|---|---:|---:|---:|---:|---:|
| cache-off/full | 5 | 13.0 | $0.032208 | 0.0% | 0.0 |
| cache-on/full | 5 | 13.0 | $0.027018 | 22.3% | 7.0 |
| cache-off/trim | 5 | 13.0 | $0.028500 | 0.0% | 0.0 |
| cache-on/trim | 5 | 13.0 | $0.026519 | 11.7% | 6.0 |

- **full history:** the optimizer changed cost per task by -16.1% ($0.032208 -> $0.027018).
- **trim history:** the optimizer changed cost per task by -7.0% ($0.028500 -> $0.026519).

## Runaway-agent guard

| Measure | Result |
|---|---|
| Seeded loops caught (stopped by the guard) | 10 of 10 (100%) |
| Call at which the guard first warned / stopped | 4.0 / 6.0 |
| Normal runs stopped (false stops) | 0 of 20 (0%) |
| Normal runs warned | 0 of 20 |
| Cost of a caught loop | $0.008268 |
| Cost of the same loop with the guard off (runs to the 30-turn limit) | $0.207369 over 30 calls |

## Limits of this measurement

- The mock's agent behavior is scripted. A real agent's normal runs include patterns the
  scripted runs don't (polling, re-running tests after a flaky failure), which is where real
  false stops would come from; the Phase 5 benchmark measures those with real models.
- The agent's system prompt and tool definitions are small (well under the 1,024-token
  minimum), so in these runs only the growing conversation is cached. Agents with large
  system prompts (Claude Code's is about 8,000 tokens) gain more from the first breakpoint.
- One task, one model. Savings depend on how many turns a task takes and how fast its
  context grows.
