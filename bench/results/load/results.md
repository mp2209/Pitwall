# Gateway overhead

How much latency Pitwall adds to a call, and LiteLLM as a baseline.

## Method

k6 ([`bench/load/loadtest.js`](../../load/loadtest.js)) sends OpenAI-format
chat completions (about 1 KB of prompt, 64 output tokens) at a fixed rate
for 30 seconds, after a 5-second warm-up. The upstream is Pitwall's mock
provider answering after a **fixed 50 ms delay**. The same load goes to the
mock directly and through each proxy; what a proxy adds is its latency
minus the direct latency at the same percentile.

- **pitwall-dev**: the gateway with no database (no keys, no budgets, no
  task tracking), the bare proxy.
- **pitwall-full**: the gateway as deployed: a virtual key checked on every
  call, a Redis budget reservation and settlement, task tracking in Redis,
  call rows written to Postgres in batches, Prometheus metrics.
- **litellm**: LiteLLM 1.111.0 proxy (`litellm[proxy]`, Python 3.14) with a
  master key and no database, the closest match to pitwall-dev. Run with its
  default single worker and with 12 workers (one per core).

Everything ran on one machine: Apple M4 Pro, 12 cores, macOS 27.2, k6
2.2.0, Pitwall at commit `9756a22`, 2026-11-24. Load generator, proxies and
mock share the CPU, so the numbers are a comparison, not a capacity claim.
Reproduce with [`bench/load/run.py`](../../load/run.py).

## Results

Added latency over calling the mock directly, in milliseconds:

| Proxy | 100/s, JSON: p50 / p99 | 500/s, JSON: p50 / p99 | 100/s, streaming: p50 / p99 |
|---|---|---|---|
| **pitwall-dev** | +0.6 / +1.1 | +0.4 / +2.2 | +0.6 / +1.3 |
| **pitwall-full** | +1.7 / +5.1 | +0.9 / +0.3 | +1.4 / +2.5 |
| litellm, 1 worker | +4.7 to +6.2 / +6.2 to +44 | 49% of requests failed; p99 13.2 s | +2,970 / +5,670 (402 of 3,000 requests never sent) |
| litellm, 12 workers | +6.9 / +31.7 | +11.5 / +2,615 (479 requests never sent) | +12.8 / +26.9 |

No Pitwall request failed in any run. "Never sent" means k6 ran out of
virtual users because responses were too slow to keep the rate.

Pitwall also measures its own overhead on every call: the time from
receiving a request to sending it upstream, recorded as `overhead_ms`. Over
about 24,500 calls each:

| | p50 | p90 | p99 |
|---|---|---|---|
| pitwall-dev | 0.11 ms | 0.17 ms | 0.30 ms |
| pitwall-full | 0.57 ms | 0.82 ms | 1.42 ms |

## Reading it

- **Pitwall adds about half a millisecond as a bare proxy and one to two
  milliseconds with keys, budgets and task tracking**, at 100 and 500
  requests a second, streaming or not. The full gateway's work before
  forwarding (key cache, task assignment and budget reservation in Redis,
  guard) is 0.6 ms at the median; the rest is the extra network hop and
  relaying the response.
- **LiteLLM's single default worker** added 5 to 6 ms at the median and up
  to 44 ms at p99 at 100 requests a second, couldn't keep up with streaming
  at that rate (median over 3 s, measured on a fresh start too), and failed
  half its requests at 500 a second.
- **With 12 workers LiteLLM kept up at 100 a second**, streaming included,
  at 7 to 13 ms added at the median, four to nine times Pitwall's full path. At
  500 a second its median stayed at +12 ms but the tail grew to seconds.
- Small p99 differences are noise: pitwall-full at 500/s came out below
  pitwall-dev. On a shared machine, a millisecond either way at p99 means
  nothing.

Raw summaries: [`main.json`](main.json) (the first table's Pitwall rows and
the LiteLLM 1-worker JSON and 500/s runs), [`litellm-1-worker-fresh.json`](litellm-1-worker-fresh.json)
(1 worker restarted, 100/s JSON and streaming) and
[`litellm-12-workers.json`](litellm-12-workers.json). The LiteLLM 1-worker
100/s JSON row is a range because the two runs gave +4.7 / +6.2 and
+6.2 / +44.
