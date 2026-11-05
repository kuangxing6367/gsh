from http.server import BaseHTTPRequestHandler, HTTPServer
import json, sys, time

PORT = int(sys.argv[1])
LOG = sys.argv[2]


def chunk(obj):
    return ('data: ' + json.dumps(obj, ensure_ascii=False) + '\n\n').encode()


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        try:
            req = json.loads(body)
        except Exception:
            req = {}
        msgs = req.get('messages', [])
        tools_given = [m for m in msgs if m.get('role') == 'tool']
        with open(LOG, 'a', encoding='utf-8') as f:
            f.write(json.dumps({
                'n_msgs': len(msgs),
                'roles': [m.get('role') for m in msgs],
                'tool_names': [m.get('name') for m in tools_given],
                'system_has_memory': any('# 长期记忆' in str(m.get('content', '')) for m in msgs if m.get('role') == 'system'),
                'ts': time.time(),
            }, ensure_ascii=False) + '\n')

        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()

        todo_done = any(m.get('name') == 'todo_write' for m in tools_given)
        bash_done = any(m.get('name') == 'bash' for m in tools_given)

        if not bash_done:
            # 第一轮：先给思考，再并发两个互不依赖的 shell 调用
            self.wfile.write(chunk({"choices": [{"delta": {"reasoning_content": "先并行跑两个命令，"}}]}))
            self.wfile.write(chunk({"choices": [{"delta": {"reasoning_content": "再汇总结果。"}}]}))
            self.wfile.write(chunk({"choices": [{"delta": {"tool_calls": [
                {"index": 0, "id": "call_a", "type": "function",
                 "function": {"name": "bash", "arguments": json.dumps({"command": "sleep 0.6; echo A"}, ensure_ascii=False)}},
            ]}}]}))
            self.wfile.write(chunk({"choices": [{"delta": {"tool_calls": [
                {"index": 1, "id": "call_b", "type": "function",
                 "function": {"name": "bash", "arguments": json.dumps({"command": "sleep 0.6; echo B"}, ensure_ascii=False)}},
            ]}}]}))
        elif not todo_done:
            # 第二轮：维护计划清单
            items = [
                {"text": "并行执行两条命令", "status": "done"},
                {"text": "汇总结果", "status": "in_progress"},
            ]
            self.wfile.write(chunk({"choices": [{"delta": {"tool_calls": [
                {"index": 0, "id": "call_todo", "type": "function",
                 "function": {"name": "todo_write", "arguments": json.dumps({"items": items}, ensure_ascii=False)}},
            ]}}]}))
        else:
            self.wfile.write(chunk({"choices": [{"delta": {"content": "两条命令都完成了。"}}]}))
            self.wfile.write(chunk({"choices": [{"delta": {}}], "usage": {"prompt_tokens": 88, "completion_tokens": 9, "total_tokens": 97}}))

        self.wfile.write(b'data: [DONE]\n\n')
        self.wfile.flush()

    def log_message(self, *a):
        pass


HTTPServer(('127.0.0.1', PORT), H).serve_forever()
