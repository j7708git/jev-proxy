"""Fake OpenAI-compatible SSE upstream for jev-proxy end-to-end testing."""
import http.server

SSE_BODY = (
    'data: {"id":"chatcmpl-e2e-1","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Proxy 是一個中介伺服器：客戶端把請求交給它，它代為轉發給目標伺服器，再把回應傳回。"}}]}\n\n'
    'data: {"id":"chatcmpl-e2e-1","model":"gpt-test","choices":[{"index":0,"delta":{"content":"常用於快取、過濾或隱藏來源。"}}]}\n\n'
    "data: [DONE]\n\n"
)


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(SSE_BODY.encode("utf-8"))

    def log_message(self, *a):
        pass


http.server.HTTPServer(("127.0.0.1", 9911), H).serve_forever()
