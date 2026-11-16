"""Smoke test: drive the gateway with the official OpenAI and Anthropic SDKs.

Each SDK talks to both a same-format and a cross-format mock model, streaming
and not, through a full tool-call round trip. The SDKs parse responses and
stream events strictly, so this catches translation bugs that unit tests
written against our own types would miss.

    pip install openai anthropic
    python scripts/sdk_smoke.py [--base-url http://localhost:8080]
"""

import argparse
import json
import sys

import anthropic
import openai

TOOLS_OPENAI = [{
    "type": "function",
    "function": {
        "name": "run_tests",
        "description": "Run the test suite",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]},
    },
}]
TOOLS_ANTHROPIC = [{
    "name": "run_tests",
    "description": "Run the test suite",
    "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]},
}]


def check(cond, msg):
    if not cond:
        raise AssertionError(msg)


def openai_round_trip(client, model, stream):
    messages = [{"role": "user", "content": "Fix the failing test"}]
    if stream:
        with client.chat.completions.stream(
            model=model, messages=messages, tools=TOOLS_OPENAI, stream_options={"include_usage": True}
        ) as s:
            for _ in s:
                pass
            final = s.get_final_completion()
        msg = final.choices[0].message
        finish = final.choices[0].finish_reason
        usage = final.usage
        check(usage is not None and usage.prompt_tokens > 0, f"no usage in stream: {usage}")
    else:
        final = client.chat.completions.create(model=model, messages=messages, tools=TOOLS_OPENAI)
        msg = final.choices[0].message
        finish = final.choices[0].finish_reason
        check(final.usage.prompt_tokens > 0, "no usage")
    check(finish == "tool_calls", f"finish_reason={finish}")
    tc = msg.tool_calls[0]
    check(tc.function.name == "run_tests", tc.function.name)
    check(json.loads(tc.function.arguments) == {"path": "mock"}, tc.function.arguments)

    messages += [
        {"role": "assistant", "content": None, "tool_calls": [tc.model_dump(exclude_none=True)]},
        {"role": "tool", "tool_call_id": tc.id, "content": "1 passed"},
    ]
    if stream:
        text = ""
        for chunk in client.chat.completions.create(model=model, messages=messages, tools=TOOLS_OPENAI, stream=True):
            if chunk.choices and chunk.choices[0].delta.content:
                text += chunk.choices[0].delta.content
    else:
        text = client.chat.completions.create(model=model, messages=messages, tools=TOOLS_OPENAI).choices[0].message.content
    check("tool result" in text, f"second turn text: {text!r}")


def anthropic_round_trip(client, model, stream):
    messages = [{"role": "user", "content": "Fix the failing test"}]
    kwargs = dict(model=model, max_tokens=512, tools=TOOLS_ANTHROPIC, system="You are a coding agent.")
    if stream:
        with client.messages.stream(messages=messages, **kwargs) as s:
            for _ in s:
                pass
            final = s.get_final_message()
    else:
        final = client.messages.create(messages=messages, **kwargs)
    check(final.stop_reason == "tool_use", f"stop_reason={final.stop_reason}")
    check(final.usage.input_tokens > 0, f"usage={final.usage}")
    tu = next(b for b in final.content if b.type == "tool_use")
    check(tu.name == "run_tests" and tu.input == {"path": "mock"}, f"tool_use={tu}")

    messages += [
        {"role": "assistant", "content": [b.model_dump(exclude_none=True) for b in final.content]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tu.id, "content": "1 passed"}]},
    ]
    if stream:
        with client.messages.stream(messages=messages, **kwargs) as s:
            text = s.get_final_text()
    else:
        text = "".join(b.text for b in client.messages.create(messages=messages, **kwargs).content if b.type == "text")
    check("tool result" in text, f"second turn text: {text!r}")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--base-url", default="http://localhost:8080")
    p.add_argument("--key", default="pw-dev")
    args = p.parse_args()

    oa = openai.OpenAI(base_url=args.base_url + "/v1", api_key=args.key, max_retries=0)
    an = anthropic.Anthropic(base_url=args.base_url, api_key=args.key, max_retries=0)
    failures = 0
    for sdk, fn, client in [("openai", openai_round_trip, oa), ("anthropic", anthropic_round_trip, an)]:
        for model in ["mock-gpt", "mock-claude"]:
            for stream in [False, True]:
                name = f"{sdk:9} sdk -> {model:11} stream={stream!s:5}"
                try:
                    fn(client, model, stream)
                    print(f"ok    {name}")
                except Exception as e:  # noqa: BLE001
                    failures += 1
                    print(f"FAIL  {name}: {type(e).__name__}: {e}")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
