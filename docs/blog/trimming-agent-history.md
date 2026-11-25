# Trimming an agent's history made it more expensive, then broke it

*November 2026. Measured with [Pitwall](../../README.md), an agent-aware AI
gateway, on 20 small coding tasks.*

Every coding agent hits the same wall: each turn resends the whole
conversation, so the prompt keeps growing. The textbook fix is to trim it.
Old tool output (a file read ten turns ago, a test log from three runs back)
gets replaced by a one-line placeholder, and the prompt stays small.

I expected trimming to save money. In my measurements it did the opposite,
and with a current model it went further: it broke the conversation.

## The setup

Pitwall sits between an agent and the model provider and groups every model
call into the task it belongs to, so it can price a whole agent run rather
than single requests. I wrote a plain agent (read files, write files, run
the tests) and ran it on 20 small Go repositories with failing tests, under
several configurations, through the gateway. The number that matters is
**cost per solved task**: total spend divided by tasks actually solved,
checked against the original tests.

The agent can keep its full history or trim all but its newest few tool
results. The gateway can add prompt-cache markers to each request, so the
provider caches the conversation so far and the next call reads it back at a
tenth of the input price.

## Surprise 1: trimming fought the cache

On a scripted nine-call agent (Pitwall's mock provider simulates Anthropic's
prompt cache), I compared full history with caching against trimming with
caching, on the same tasks:

| | Tokens sent per solved task | Read from cache | Cost per solved task |
|---|---|---|---|
| Full history, cached | 19.2k | 71% | $0.0296 |
| Trimmed every turn, cached | 16.6k | 36% | $0.0448 |

Trimming sent 14% fewer tokens and cost **51% more**.

The reason is how prompt caching works. The cache stores a prefix of the
prompt, and a later call gets a hit only if its prompt starts with exactly
that prefix. Trimming every turn edits an older message, say the result of
call 4, on every call. So the prefix the previous call cached no longer
matches anything after call 4. Each call reads back only the system prompt
and tools, and pays to write the rewritten history into the cache again.
That write costs 1.25 times plain input. Keeping the history unchanged is
cheaper than shortening it, as long as the unchanged part is cached.

The fix is to edit less often. I changed the agent to move its trim point
four results at a time instead of one. The history then stays identical for
three calls out of four, and those calls read the cache. That brought the
cost to $0.0321: 28% cheaper than trimming every turn, and within 9% of
keeping everything.

## Surprise 2: trimming broke the conversation

Then I ran the same configurations on real models: Claude Haiku, Sonnet and
Opus 5.5, 360 runs, $4.98 in total. The tasks turned out to be easy. Every
configuration solved every task in three to five calls, so the trimming
configurations barely got to trim. When they did, something new happened.

In 27 of 60 runs with per-turn trimming, Sonnet's API refused the agent's
next call:

> Invalid `signature` in `thinking` block. The block is bound to a different
> conversation. ... Content before this block differs from when it was
> created.

Sonnet 5.5 returns its reasoning as signed thinking blocks, and the agent
has to send them back on later turns. Each block is bound to the
conversation before it. Trim a tool result that came earlier, and every
thinking block after it no longer matches its history, so the request is
rejected. Here it only hit the agent's last call, after the fix was
written, so those runs still counted as solved. On a longer task it would
end the run.

The error message names the two ways out: drop the thinking blocks that
come after the edit, or opt in to letting the API drop mismatched blocks
itself. Either way, editing history is no longer just a token
optimization. It changes what the model can see of its own reasoning, and
the API checks it.

## What I'd do

- **Let the cache absorb the repetition first.** On these tasks, full
  history with cache markers was the cheapest Sonnet setup. Caching cut
  input cost by half and total cost by 31%, and needed no change to the
  agent.
- **If you trim, trim in batches.** Move the trim point rarely, so most
  calls still hit the cache.
- **Treat thinking blocks as part of the history you're editing.** Drop the
  ones after the edit, or use the provider's option for it, and test
  trimming against a model that returns them.
- **Measure per task, not per request.** Per request, trimming looked like a
  win: fewer tokens on every call. Per task, it was a loss, and the
  failures only showed up in the task view.

## Caveats

The tasks are small and the runs short, so the real-model run says more
about cost overhead than about long sessions, where trimming matters most.
The cache numbers in the first table come from the mock provider's
simulation of Anthropic's cache, run against the gateway's real pricing.
The full method, every configuration and the raw runs are in the
[findings](../../bench/results/findings.md).
