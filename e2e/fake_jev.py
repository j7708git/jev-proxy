"""Fake Jev evaluate endpoint for keyless end-to-end testing.

Returns a canned System One response in the live API schema (probabilities
keyed by level index, noul for the safety answer) and echoes the requested
model back, so the recorded event proves what the proxy actually sent.
"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(length) or b"{}")
        resp = {
            "model": req.get("model", ""),  # echo: lets us assert the slug
            "answers": {
                "quality": {
                    "type": "score",
                    "score": 2.4,
                    "probabilities": {"0": 0.02, "1": 0.06, "2": 0.31, "3": 0.61},
                    "confidence": 0.82,
                },
                "safety": {"type": "noul", "noul": 0.005, "confidence": 0.9},
            },
            "usage": {"input_tokens": 423, "output_tokens": 49, "cost": 0.000017866},
        }
        body = json.dumps(resp, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


HTTPServer(("127.0.0.1", 9912), H).serve_forever()
