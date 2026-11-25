// Load test for gateway overhead: a fixed request rate of OpenAI-format chat
// completions against a target, which is either the mock provider itself
// (the baseline) or a gateway in front of it. The mock answers after a fixed
// delay, so the difference between going through a gateway and calling the
// mock directly is the gateway's overhead.
//
//   k6 run -e TARGET=http://127.0.0.1:8380 -e RATE=200 -e SUMMARY=out.json bench/load/loadtest.js
//
// Environment: TARGET (base URL), KEY (bearer token, optional), MODEL
// (default mock-gpt), RATE (requests per second, default 100), DURATION
// (default 30s), STREAM (1 for streaming), SUMMARY (where to write the JSON
// summary).
import http from "k6/http";
import { check } from "k6";

const rate = Number(__ENV.RATE || 100);

export const options = {
  discardResponseBodies: false,
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
  scenarios: {
    load: {
      executor: "constant-arrival-rate",
      rate: rate,
      timeUnit: "1s",
      duration: __ENV.DURATION || "30s",
      preAllocatedVUs: Math.max(20, rate),
      maxVUs: Math.max(100, rate * 4),
    },
  },
};

// About 1 KB of prompt: a system message and a short coding question, the
// shape of a small agent turn.
const body = JSON.stringify({
  model: __ENV.MODEL || "mock-gpt",
  max_tokens: 64,
  stream: __ENV.STREAM === "1",
  messages: [
    { role: "system", content: "You are a careful coding agent working in a Go repository. ".repeat(8) },
    { role: "user", content: "The tests in wordcount fail: Count splits on single spaces only. Suggest a fix." },
  ],
});

const params = {
  headers: Object.assign(
    { "Content-Type": "application/json" },
    __ENV.KEY ? { Authorization: "Bearer " + __ENV.KEY } : {},
  ),
  timeout: "30s",
};

export default function () {
  const res = http.post(__ENV.TARGET + "/v1/chat/completions", body, params);
  check(res, {
    "status 200": (r) => r.status === 200,
    "has content": (r) => r.body && r.body.length > 0,
  });
}

export function handleSummary(data) {
  const d = data.metrics.http_req_duration.values;
  const out = {
    target: __ENV.TARGET,
    rate: rate,
    stream: __ENV.STREAM === "1",
    requests: data.metrics.http_reqs.values.count,
    failed_rate: data.metrics.http_req_failed.values.rate,
    dropped: data.metrics.dropped_iterations ? data.metrics.dropped_iterations.values.count : 0,
    ms: { avg: d.avg, p50: d.med, p90: d["p(90)"], p95: d["p(95)"], p99: d["p(99)"], max: d.max },
  };
  const files = { stdout: JSON.stringify(out) + "\n" };
  if (__ENV.SUMMARY) files[__ENV.SUMMARY] = JSON.stringify(out, null, 2);
  return files;
}
