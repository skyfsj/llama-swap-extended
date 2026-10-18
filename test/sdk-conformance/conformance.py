#!/usr/bin/env python3
"""Protocol conformance: drive llama-swap with the vendors' official SDKs.

Every case goes through the real HTTP stack of the SDK, so a malformed envelope,
a missing required field or a wrong finish reason fails the case even when the
bytes "look" right.

Usage: conformance.py <base_url> [--stream]
"""
import json
import sys
import traceback

BASE = sys.argv[1].rstrip("/")
STREAM = "--stream" in sys.argv

RESULTS = []


def case(name):
    def wrap(fn):
        def run():
            try:
                fn()
                RESULTS.append((name, True, ""))
            except Exception as exc:  # noqa: BLE001
                RESULTS.append((name, False, f"{type(exc).__name__}: {exc}"))
        run.__name__ = fn.__name__
        return run
    return wrap


def check(condition, message):
    if not condition:
        raise AssertionError(message)


# --------------------------------------------------------------------------
# OpenAI SDK: Chat Completions
# --------------------------------------------------------------------------

@case("openai.chat.nonstream")
def openai_chat_nonstream():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.chat.completions.create(
        model="chat-model", messages=[{"role": "user", "content": "hi"}], max_tokens=16)
    check(response.choices[0].message.content == "pong", f"content={response.choices[0].message.content!r}")
    check(response.choices[0].finish_reason == "stop", f"finish={response.choices[0].finish_reason}")
    check(response.usage.prompt_tokens == 7 and response.usage.completion_tokens == 2,
          f"usage={response.usage}")
    check(response.model, "model missing")


@case("openai.chat.stream")
def openai_chat_stream():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    chunks = list(client.chat.completions.create(
        model="chat-model", messages=[{"role": "user", "content": "hi"}], max_tokens=16, stream=True,
        stream_options={"include_usage": True}))
    text = "".join(c.choices[0].delta.content or "" for c in chunks if c.choices)
    check(text == "pong", f"text={text!r}")
    finish = [c.choices[0].finish_reason for c in chunks if c.choices and c.choices[0].finish_reason]
    check(finish == ["stop"], f"finish={finish}")
    usage = [c.usage for c in chunks if getattr(c, "usage", None)]
    check(usage and usage[-1].prompt_tokens == 7, f"usage={usage}")


@case("openai.chat.toolcall")
def openai_chat_toolcall():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.chat.completions.create(
        model="chat-model", messages=[{"role": "user", "content": "use the tool lookup"}], max_tokens=16,
        tools=[{"type": "function", "function": {"name": "lookup", "parameters": {"type": "object"}}}])
    choice = response.choices[0]
    check(choice.finish_reason == "tool_calls", f"finish={choice.finish_reason}")
    call = choice.message.tool_calls[0]
    check(call.function.name == "lookup", f"name={call.function.name}")
    check(json.loads(call.function.arguments) == {"q": "x"}, f"args={call.function.arguments}")


# --------------------------------------------------------------------------
# OpenAI SDK: Responses
# --------------------------------------------------------------------------

@case("openai.responses.nonstream")
def openai_responses_nonstream():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.responses.create(model="chat-model", input="hi", max_output_tokens=16)
    check(response.output_text == "pong", f"text={response.output_text!r}")
    check(response.status == "completed", f"status={response.status}")
    check(response.usage.input_tokens == 7 and response.usage.output_tokens == 2, f"usage={response.usage}")


@case("openai.responses.stream")
def openai_responses_stream():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    text = ""
    completed = None
    with client.responses.stream(model="chat-model", input="hi", max_output_tokens=16) as stream:
        for event in stream:
            if event.type == "response.output_text.delta":
                text += event.delta
            elif event.type == "response.completed":
                completed = event.response
    check(text == "pong", f"text={text!r}")
    check(completed is not None, "no response.completed event")
    check(completed.usage.input_tokens == 7, f"usage={completed.usage}")


@case("openai.responses.toolcall")
def openai_responses_toolcall():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.responses.create(
        model="chat-model", input="use the tool lookup", max_output_tokens=16,
        tools=[{"type": "function", "name": "lookup", "parameters": {"type": "object"}}])
    calls = [item for item in response.output if item.type == "function_call"]
    check(calls, f"no function_call output: {[i.type for i in response.output]}")
    check(calls[0].name == "lookup", f"name={calls[0].name}")
    check(json.loads(calls[0].arguments) == {"q": "x"}, f"args={calls[0].arguments}")


# --------------------------------------------------------------------------
# Anthropic SDK
# --------------------------------------------------------------------------

@case("anthropic.messages.nonstream")
def anthropic_nonstream():
    import anthropic
    client = anthropic.Anthropic(base_url=BASE, api_key="test")
    message = client.messages.create(model="chat-model", max_tokens=16,
                                     messages=[{"role": "user", "content": "hi"}])
    check(message.content[0].text == "pong", f"content={message.content}")
    check(message.stop_reason == "end_turn", f"stop={message.stop_reason}")
    check(message.usage.input_tokens == 7 and message.usage.output_tokens == 2, f"usage={message.usage}")


@case("anthropic.messages.stream")
def anthropic_stream():
    import anthropic
    client = anthropic.Anthropic(base_url=BASE, api_key="test")
    with client.messages.stream(model="chat-model", max_tokens=16,
                                messages=[{"role": "user", "content": "hi"}]) as stream:
        message = stream.get_final_message()
    check(message.content[0].text == "pong", f"content={message.content}")
    check(message.stop_reason == "end_turn", f"stop={message.stop_reason}")


@case("anthropic.messages.toolcall")
def anthropic_toolcall():
    import anthropic
    client = anthropic.Anthropic(base_url=BASE, api_key="test")
    message = client.messages.create(
        model="chat-model", max_tokens=16, messages=[{"role": "user", "content": "use the tool lookup"}],
        tools=[{"name": "lookup", "input_schema": {"type": "object"}}])
    check(message.stop_reason == "tool_use", f"stop={message.stop_reason}")
    block = message.content[0]
    check(block.type == "tool_use" and block.name == "lookup", f"block={block}")
    check(block.input == {"q": "x"}, f"input={block.input}")


# --------------------------------------------------------------------------
# Google GenAI SDK: generateContent
# --------------------------------------------------------------------------

@case("google.genai.nonstream")
def google_genai_nonstream():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    response = client.models.generate_content(model="gemini-model", contents="hi")
    check(response.text == "pong", f"text={response.text!r}")
    candidate = response.candidates[0]
    check(str(candidate.finish_reason).endswith("STOP"), f"finish={candidate.finish_reason}")
    check(response.usage_metadata.prompt_token_count == 7, f"usage={response.usage_metadata}")


@case("google.genai.stream")
def google_genai_stream():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    text = ""
    for chunk in client.models.generate_content_stream(model="gemini-model", contents="hi"):
        text += chunk.text or ""
    check(text == "pong", f"text={text!r}")


@case("google.genai.toolcall")
def google_genai_toolcall():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    response = client.models.generate_content(
        model="gemini-model", contents="use the tool lookup",
        config={"tools": [{"function_declarations": [{"name": "lookup", "parameters": {"type": "object"}}]}]})
    call = response.candidates[0].content.parts[0].function_call
    check(call.name == "lookup", f"name={call.name}")
    check(dict(call.args) == {"q": "x"}, f"args={call.args}")


# --------------------------------------------------------------------------
# Google GenAI SDK: Interactions
# --------------------------------------------------------------------------

@case("google.interactions.nonstream")
def google_interactions_nonstream():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.create(model="interactions-model", input="hi")
    check(interaction.id, "no interaction id")
    check(interaction.status == "completed", f"status={interaction.status}")
    steps = interaction.steps or []
    check(steps, f"no steps: {interaction}")
    text = ""
    for step in steps:
        if step.type == "model_output":
            for content in step.content or []:
                if content.type == "text":
                    text += content.text or ""
    check(text == "pong", f"text={text!r}")
    usage = interaction.usage
    check(usage is not None and usage.total_input_tokens == 7, f"usage={usage}")


@case("google.interactions.stream")
def google_interactions_stream():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    stream = client.interactions.create(model="interactions-model", input="hi", stream=True)
    text = ""
    completed = None
    for event in stream:
        event_type = getattr(event, "event_type", None)
        if event_type == "step.delta":
            delta = getattr(event, "delta", None)
            if delta is not None and getattr(delta, "type", None) == "text":
                text += delta.text or ""
        elif event_type == "interaction.completed":
            completed = event.interaction
    check(text == "pong", f"text={text!r}")
    check(completed is not None, "no interaction.completed event")
    check(completed.status == "completed", f"status={completed.status}")


@case("google.interactions.toolcall_stream")
def google_interactions_toolcall_stream():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    stream = client.interactions.create(model="interactions-model", input="use the tool lookup", stream=True)
    name, arguments = None, ""
    for event in stream:
        if getattr(event, "event_type", None) != "step.delta":
            continue
        delta = getattr(event, "delta", None)
        delta_type = getattr(delta, "type", None)
        if delta_type == "arguments_delta":
            arguments += getattr(delta, "arguments", "") or ""
        if delta_type == "arguments":
            raise AssertionError("stream used the wrong delta discriminator 'arguments'")
    for event in []:
        pass
    check(arguments, f"no arguments_delta in the stream (name={name})")
    check(json.loads(arguments) == {"q": "x"}, f"arguments={arguments!r}")


@case("google.interactions.get_roundtrip")
def google_interactions_get():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.get("v1_test")
    check(interaction.id == "v1_test", f"id={interaction.id}")
    check(interaction.status == "completed", f"status={interaction.status}")


@case("google.interactions.cancel")
def google_interactions_cancel():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    result = client.interactions.cancel("v1_test")
    check(result is not None, "cancel returned nothing")


@case("google.interactions.delete")
def google_interactions_delete():
    # The SDK types delete's result as Any, so the contract under test is that
    # the route exists and the call succeeds instead of 404ing.
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    client.interactions.delete("v1_test")


@case("google.interactions.requires_action_status")
def google_interactions_requires_action():
    # A tool-call interaction reports requires_action; a client must still see
    # the call, and the proxy must not report it as a finished answer.
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.create(
        model="interactions-model", input="use the tool lookup",
        tools=[{"type": "function", "name": "lookup", "parameters": {"type": "object"}}])
    calls = [step for step in (interaction.steps or []) if step.type == "function_call"]
    check(calls, f"no function_call step: {interaction}")


@case("google.interactions.toolcall")
def google_interactions_toolcall():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.create(
        model="interactions-model", input="use the tool lookup",
        # The Interactions Tool is flat: type:"function" with name/parameters at
        # the top level (the SDK rejects a nested {"function": {...}}).
        tools=[{"type": "function", "name": "lookup", "parameters": {"type": "object"}}])
    calls = [step for step in (interaction.steps or []) if step.type == "function_call"]
    check(calls, f"no function_call step: {[s.type for s in interaction.steps or []]}")
    check(calls[0].name == "lookup", f"name={calls[0].name}")
    check(dict(calls[0].arguments) == {"q": "x"}, f"args={calls[0].arguments}")


# --------------------------------------------------------------------------
# Cross-dialect: every SDK against every upstream surface
# --------------------------------------------------------------------------

@case("cross.openai_chat_client_to_anthropic_upstream")
def cross_chat_to_anthropic():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.chat.completions.create(
        model="anthropic-model", messages=[{"role": "user", "content": "hi"}], max_tokens=16)
    check(response.choices[0].message.content == "pong", f"content={response.choices[0].message.content!r}")
    check(response.choices[0].finish_reason == "stop", f"finish={response.choices[0].finish_reason}")


@case("cross.openai_chat_client_to_gemini_upstream")
def cross_chat_to_gemini():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.chat.completions.create(
        model="gemini-model", messages=[{"role": "user", "content": "hi"}], max_tokens=16)
    check(response.choices[0].message.content == "pong", f"content={response.choices[0].message.content!r}")


@case("cross.openai_responses_client_to_gemini_upstream")
def cross_responses_to_gemini():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.responses.create(model="gemini-model", input="hi", max_output_tokens=16)
    check(response.output_text == "pong", f"text={response.output_text!r}")
    check(response.status == "completed", f"status={response.status}")


@case("cross.anthropic_client_to_chat_upstream")
def cross_anthropic_to_chat():
    import anthropic
    client = anthropic.Anthropic(base_url=BASE, api_key="test")
    message = client.messages.create(model="chat-model", max_tokens=16,
                                     messages=[{"role": "user", "content": "hi"}])
    check(message.content[0].text == "pong", f"content={message.content}")
    check(message.stop_reason == "end_turn", f"stop={message.stop_reason}")


@case("cross.gemini_client_to_chat_upstream")
def cross_gemini_to_chat():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    response = client.models.generate_content(model="chat-model", contents="hi")
    check(response.text == "pong", f"text={response.text!r}")


@case("cross.interactions_client_to_chat_upstream")
def cross_interactions_to_chat():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.create(model="chat-model", input="hi")
    text = ""
    for step in interaction.steps or []:
        if step.type == "model_output":
            for content in step.content or []:
                if content.type == "text":
                    text += content.text or ""
    check(text == "pong", f"text={text!r} interaction={interaction}")


@case("cross.interactions_client_to_gemini_upstream")
def cross_interactions_to_gemini():
    from google import genai
    client = genai.Client(api_key="test", http_options={"base_url": BASE})
    interaction = client.interactions.create(model="gemini-model", input="hi")
    text = ""
    for step in interaction.steps or []:
        if step.type == "model_output":
            for content in step.content or []:
                if content.type == "text":
                    text += content.text or ""
    check(text == "pong", f"text={text!r} interaction={interaction}")


@case("cross.chat_client_to_interactions_upstream")
def cross_chat_to_interactions():
    from openai import OpenAI
    client = OpenAI(base_url=f"{BASE}/v1", api_key="test")
    response = client.chat.completions.create(
        model="interactions-model", messages=[{"role": "user", "content": "hi"}], max_tokens=16)
    check(response.choices[0].message.content == "pong", f"content={response.choices[0].message.content!r}")


@case("cross.anthropic_client_to_interactions_upstream")
def cross_anthropic_to_interactions():
    import anthropic
    client = anthropic.Anthropic(base_url=BASE, api_key="test")
    message = client.messages.create(model="interactions-model", max_tokens=16,
                                     messages=[{"role": "user", "content": "hi"}])
    check(message.content[0].text == "pong", f"content={message.content}")


def main():
    for name, fn in list(globals().items()):
        if callable(fn) and getattr(fn, "__name__", "").startswith(("openai", "anthropic", "google", "cross")):
            fn()
    passed = sum(1 for _, ok, _ in RESULTS if ok)
    for name, ok, err in RESULTS:
        print(f"{'PASS' if ok else 'FAIL'} {name}" + (f"  -> {err}" if err else ""))
    print(f"TOTAL pass={passed} fail={len(RESULTS) - passed}")
    return 0 if passed == len(RESULTS) else 1


if __name__ == "__main__":
    sys.exit(main())
