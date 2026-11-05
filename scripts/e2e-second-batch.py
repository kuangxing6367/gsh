"""第二批能力的端到端验证用伪模型（SSE）。

它不聪明，但会按剧本把每条新链路都走一遍：
  1. code_search  + todo_write 并发
  2. task（子 agent 派活）
  3. bash（被钩子拦截）
  4. 收尾

运行：python scripts/e2e-second-batch.py <port> <log.jsonl>
子任务识别：system 里出现「# 子任务」就当作子 agent，直接给结论。
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json, sys, time

PORT = int(sys.argv[1])
LOG = sys.argv[2]


def chunk(obj):
    return ('data: ' + json.dumps(obj, ensure_ascii=False) + '\n\n').encode()


def sys_text(msgs):
    return ''.join(str(m.get('content') or '') for m in msgs if m.get('role') == 'system')


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        try:
            req = json.loads(body)
        except Exception:
            req = {}
        msgs = req.get('messages', [])
        tool_msgs = [m for m in msgs if m.get('role') == 'tool']
        names = [m.get('name') for m in tool_msgs]
        system = sys_text(msgs)

        with open(LOG, 'a', encoding='utf-8') as f:
            f.write(json.dumps({
                'roles': [m.get('role') for m in msgs],
                'tools_seen': names,
                'is_subagent': '# 子任务' in system,
                'system_has_index': '# 代码索引' in system,
                'system_has_memory': '# 长期记忆' in system,
                'system_has_plan': '# 计划模式' in system,
                'image_parts': sum(
                    1 for m in msgs
                    if isinstance(m.get('content'), list)
                    and any(p.get('type') == 'image_url' for p in m['content'])
                ),
                'ts': time.time(),
            }, ensure_ascii=False) + '\n')

        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()

        def send(obj):
            self.wfile.write(chunk(obj))

        def finish(text):
            send({"choices": [{"delta": {"content": text}}]})
            send({"choices": [{"delta": {}}], "usage": {"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}})

        # 子 agent：不再调工具，直接给结论。
        if '# 子任务' in system:
            finish('子任务结论：鉴权在 internal/tools 里。')
            self.wfile.write(b'data: [DONE]\n\n')
            self.wfile.flush()
            return

        if 'code_search' not in names and 'todo_write' not in names:
            send({"choices": [{"delta": {"tool_calls": [
                {"index": 0, "id": "call_cs", "type": "function",
                 "function": {"name": "code_search", "arguments": json.dumps({"query": "RegisterKernel"})}},
            ]}}]})
            send({"choices": [{"delta": {"tool_calls": [
                {"index": 1, "id": "call_todo", "type": "function",
                 "function": {"name": "todo_write", "arguments": json.dumps({"items": [
                     {"text": "查索引", "status": "done"},
                     {"text": "派子任务", "status": "in_progress"},
                 ]})}},
            ]}}]})
        elif 'task' not in names:
            send({"choices": [{"delta": {"tool_calls": [
                {"index": 0, "id": "call_task", "type": "function",
                 "function": {"name": "task", "arguments": json.dumps({"prompt": "鉴权实现在哪？"})}},
            ]}}]})
        elif 'bash' not in names:
            send({"choices": [{"delta": {"tool_calls": [
                {"index": 0, "id": "call_bash", "type": "function",
                 "function": {"name": "bash", "arguments": json.dumps({"command": "echo 应该被拦住"})}},
            ]}}]})
        else:
            blocked = any('hook' in str(m.get('content', '')) for m in tool_msgs)
            finish('全部完成。钩子拦截生效=%s' % ('是' if blocked else '否'))

        self.wfile.write(b'data: [DONE]\n\n')
        self.wfile.flush()

    def log_message(self, *a):
        pass


HTTPServer(('127.0.0.1', PORT), H).serve_forever()
