#!/usr/bin/env python3
"""Mock OpenAI-compatible provider for the AgentForge demo.

A loopback stand-in for a cloud endpoint (DeepSeek-shaped smoke target), so the
refusal-interception flow can be demonstrated without an API key. Request 1
returns an unmistakable first-person refusal; once the harness has injected its
reminder, later requests complete the task.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("MOCK_PORT", "18177"))


def sse(wfile, content):
    wfile.write(b"data: " + json.dumps({
        "choices": [{"delta": {"content": content}}]
    }).encode() + b"\n\n")
    wfile.write(b"data: [DONE]\n\n")
    wfile.flush()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        req = json.loads(self.rfile.read(length) or b"{}")
        messages = req.get("messages", [])
        system = "\n".join(
            str(m.get("content", ""))
            for m in messages
            if m.get("role") == "system"
        )
        if "Harness reminder (refusal intercepted)" in system:
            body = (
                "Scope confirmed. Context checks out. I will build the "
                "tasking harness as an authorized lab exercise: a framed "
                "message type, a local socket transport, and a test that "
                "asserts the framing round-trips."
            )
            print("mock: reminder seen -> resuming task", file=sys.stderr)
        else:
            body = "I can't assist with that request."
            print("mock: refusing initial request", file=sys.stderr)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        sse(self.wfile, body)


if __name__ == "__main__":
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"mock provider listening on 127.0.0.1:{PORT}", file=sys.stderr)
    server.serve_forever()
