#!/usr/bin/env python3
"""Fake vendor upstreams for protocol-conformance testing.

Each server speaks one vendor dialect on its real paths and answers both
non-streaming and streaming requests, including a tool call on demand
(triggered by the word "tool" in the prompt).
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
MODE = sys.argv[2]  # chat | anthropic | gemini | interactions


def wants_tool(body: dict) -> bool:
    text = json.dumps(body).lower()
    return '"tool"' in text or "use the tool" in text or "lookup" in text


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _send(self, obj, status=200, ctype="application/json"):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _sse(self, frames):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for frame in frames:
            self.wfile.write(frame.encode())
            self.wfile.flush()

    def _interaction_id(self):
        # The SDK appends ?stream=false to retrieval paths.
        path = self.path.split("?", 1)[0].rstrip("/")
        return path.rsplit("/", 1)[-1]

    def do_GET(self):
        if self.path.rstrip("/") in ("/health", "/healthz"):
            self._send({"status": "ok"})
            return
        if "/interactions/" in self.path:
            self._send({"id": self._interaction_id(), "object": "interaction",
                        "status": "completed", "model": "m",
                        "created": "2026-10-17T12:00:00Z", "updated": "2026-10-17T12:00:01Z",
                        "steps": [{"type": "model_output", "content": [{"type": "text", "text": "pong"}]}],
                        "usage": {"total_input_tokens": 7, "total_output_tokens": 2, "total_tokens": 9}})
            return
        if self.path.endswith("/models"):
            self._send({"object": "list", "data": [{"id": "m", "object": "model"}]})
            return
        self._send({"error": "not found"}, 404)

    def do_DELETE(self):
        if "/interactions/" in self.path:
            self._send({"id": self._interaction_id(), "object": "interaction", "deleted": True})
            return
        self._send({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) or b"{}"
        try:
            body = json.loads(raw)
        except Exception:
            self._send({"error": {"message": "invalid json"}}, 400)
            return
        stream = bool(body.get("stream")) or "alt=sse" in self.path
        tool = wants_tool(body)
        handler = {
            "chat": self.chat,
            "anthropic": self.anthropic,
            "gemini": self.gemini,
            "interactions": self.interactions,
        }[MODE]
        handler(body, stream, tool)

    # --- OpenAI Chat Completions ---
    def chat(self, body, stream, tool):
        if stream:
            frames = []
            if tool:
                frames.append('data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\\"q\\":\\"x\\"}"}}]}}]}\n\n')
                frames.append('data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}\n\n')
            else:
                frames.append('data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"pong"}}]}\n\n')
                frames.append('data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n')
            frames.append('data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}\n\n')
            frames.append("data: [DONE]\n\n")
            self._sse(frames)
            return
        if tool:
            self._send({"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": None,
                                     "tool_calls": [{"id": "call_1", "type": "function",
                                                     "function": {"name": "lookup", "arguments": '{"q":"x"}'}}]},
                                     "finish_reason": "tool_calls"}],
                        "usage": {"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9}})
            return
        self._send({"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "pong"},
                                 "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9}})

    # --- Anthropic Messages ---
    def anthropic(self, body, stream, tool):
        if stream:
            frames = [
                'event: message_start\ndata: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}\n\n',
            ]
            if tool:
                frames.append('event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}\n\n')
                frames.append('event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\\"q\\":\\"x\\"}"}}\n\n')
                frames.append('event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n')
                frames.append('event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}\n\n')
            else:
                frames.append('event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n')
                frames.append('event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}\n\n')
                frames.append('event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n')
                frames.append('event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}\n\n')
            frames.append('event: message_stop\ndata: {"type":"message_stop"}\n\n')
            self._sse(frames)
            return
        if tool:
            self._send({"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
                        "content": [{"type": "tool_use", "id": "call_1", "name": "lookup", "input": {"q": "x"}}],
                        "stop_reason": "tool_use", "usage": {"input_tokens": 7, "output_tokens": 2}})
            return
        self._send({"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
                    "content": [{"type": "text", "text": "pong"}],
                    "stop_reason": "end_turn", "usage": {"input_tokens": 7, "output_tokens": 2}})

    # --- Gemini generateContent ---
    def gemini(self, body, stream, tool):
        if tool:
            parts = [{"functionCall": {"name": "lookup", "args": {"q": "x"}}, "thoughtSignature": "U0lHLVVQ"}]
        else:
            parts = [{"text": "pong"}]
        payload = {"candidates": [{"content": {"role": "model", "parts": parts},
                                   "finishReason": "STOP", "index": 0}],
                   "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 2, "totalTokenCount": 9}}
        if stream:
            self._sse([f"data: {json.dumps(payload)}\n\n"])
            return
        self._send(payload)

    # --- Gemini Interactions ---
    def interactions(self, body, stream, tool):
        steps = ([{"type": "function_call", "id": "call_1", "name": "lookup", "arguments": {"q": "x"}}]
                 if tool else [{"type": "model_output", "content": [{"type": "text", "text": "pong"}]}])
        usage = {"total_input_tokens": 7, "total_output_tokens": 2, "total_tokens": 9}
        status = "requires_action" if tool else "completed"
        interaction = {"id": "v1_test", "object": "interaction", "status": status,
                       "model": "m", "created": "2026-10-17T12:00:00Z", "updated": "2026-10-17T12:00:01Z",
                       "steps": steps, "usage": usage}
        if stream:
            frames = [f'event: interaction.created\ndata: {json.dumps({"event_type":"interaction.created","interaction":{"id":"v1_test","object":"interaction","status":"in_progress","model":"m","created":"2026-10-17T12:00:00Z"}})}\n\n']
            if tool:
                frames.append(f'event: step.start\ndata: {json.dumps({"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1","name":"lookup","arguments":{}}})}\n\n')
                args_delta = json.dumps({"event_type": "step.delta", "index": 0,
                                        "delta": {"type": "arguments_delta", "arguments": '{"q":"x"}'}})
                frames.append("event: step.delta\ndata: " + args_delta + "\n\n")
                frames.append(f'event: step.stop\ndata: {json.dumps({"event_type":"step.stop","index":0})}\n\n')
            else:
                frames.append(f'event: step.start\ndata: {json.dumps({"event_type":"step.start","index":0,"step":{"type":"model_output","content":[]}})}\n\n')
                frames.append(f'event: step.delta\ndata: {json.dumps({"event_type":"step.delta","index":0,"delta":{"type":"text","text":"pong"}})}\n\n')
                frames.append(f'event: step.stop\ndata: {json.dumps({"event_type":"step.stop","index":0,"usage":usage})}\n\n')
            frames.append(f'event: interaction.completed\ndata: {json.dumps({"event_type":"interaction.completed","interaction":interaction})}\n\n')
            self._sse(frames)
            return
        self._send(interaction)


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
