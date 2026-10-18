"""Recording mock OpenAI-compatible model for the Orbit red-team.

It rejects the first (tool-calling) request with a 400 so the Orbit client
falls back to its snapshot path, then records exactly what Orbit sends on the
snapshot request. The test then asserts no secret-named field value appears in
what was recorded. No real model, no cost.
"""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.environ.get("MODEL_LOG", "/tmp/orbit-model-requests.log")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith("/models"):
            return self._json(200, {"object": "list", "data": [{"id": "mock", "object": "model"}]})
        self._json(404, {"error": "unsupported"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        # First call carries "tools"; reject so the client uses the snapshot path.
        if body.get("tools"):
            return self._json(400, {"error": {"message": "this model does not support tools or function calling"}})
        # Snapshot call: record exactly what Orbit sent, then answer trivially.
        try:
            with open(LOG, "ab") as f:
                f.write(raw + b"\n")
        except Exception:
            pass
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            model = body.get("model", "mock")
            chunk = {"id": "mock", "object": "chat.completion.chunk", "model": model,
                     "choices": [{"index": 0, "delta": {"role": "assistant", "content": "ok"}, "finish_reason": None}]}
            self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
            final = {"id": "mock", "object": "chat.completion.chunk", "model": model,
                     "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
            self.wfile.write(b"data: " + json.dumps(final).encode() + b"\n\ndata: [DONE]\n\n")
            return
        self._json(200, {"id": "mock", "object": "chat.completion", "model": body.get("model", "mock"),
                         "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                         "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 7071), Handler).serve_forever()
