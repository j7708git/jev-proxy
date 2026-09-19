"""Fake OpenAI-compatible upstream for jev-proxy end-to-end testing.

POST (any path): streams a canned SSE reply. GET (any path): canned /models
list, so gateway-mode model aggregation can be exercised without keys.
Port via argv[1] (default 9911) — gateway e2e launches two instances.
"""
import http.server
import json
import sys

SSE_BODY = (
    'data: {"id":"chatcmpl-e2e-1","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Proxy 是一個中介伺服器：客戶端把請求交給它，它代為轉發給目標伺服器，再把回應傳回。"}}]}\n\n'
    'data: {"id":"chatcmpl-e2e-1","model":"gpt-test","choices":[{"index":0,"delta":{"content":"常用於快取、過濾或隱藏來源。"}}]}\n\n'
    "data: [DONE]\n\n"
)
MODELS_BODY = json.dumps(
    {"object": "list", "data": [{"id": "gpt-test"}, {"id": "deepseek/deepseek-chat-v3"}]}
)


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(SSE_BODY.encode("utf-8"))

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(MODELS_BODY.encode())

    def log_message(self, *a):
        pass


port = int(sys.argv[1]) if len(sys.argv) > 1 else 9911
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
