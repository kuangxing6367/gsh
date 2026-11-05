#!/usr/bin/env python3
"""端到端冒烟：用伪模型驱动真实 gsh 进程，检查工具层的行为。

伪模型只做两件事：
  1. 第一轮返回环境变量指定的工具调用；
  2. 收到工具结果后原样回显，让人能在 stdout 里看到真实返回值。

这样可以绕开真实 provider，专注验证「工具到底有没有被执行 / 被拦」。
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

TOOL = os.environ.get("SMOKE_TOOL", "read_file")
ARGS = os.environ.get("SMOKE_ARGS", '{"path":"README.md"}')


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        msgs = body.get("messages", [])
        tool_msgs = [m for m in msgs if m.get("role") == "tool"]
        names = sorted(t["function"]["name"] for t in body.get("tools", []))

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        if not tool_msgs:
            chunk = {
                "choices": [{
                    "delta": {
                        "tool_calls": [{
                            "index": 0, "id": "c1", "type": "function",
                            "function": {"name": TOOL, "arguments": ARGS},
                        }]
                    }
                }]
            }
        else:
            got = str(tool_msgs[-1].get("content", ""))
            chunk = {
                "choices": [{"delta": {"content":
                    "TOOLS=" + ",".join(names) + "\n"
                    "RESULT<<<\n" + got[:600] + "\n>>>END"
                }}]
            }
        self.wfile.write(("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode())
        self.wfile.write(b"data: [DONE]\n\n")


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8951
    HTTPServer(("127.0.0.1", port), H).serve_forever()
