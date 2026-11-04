# 会话管理

会话以 append-only JSONL 落盘，是 gsh 的第一公民。

## 恢复 / 分叉

- `-s <id>`：进入终端 UI 并定位到该会话，回放历史后继续提问。
- `--fork <id>`：以某会话为蓝本复制一份新会话后进入（原会话不受影响）。
- 终端内 `Ctrl+P` 打开会话列表，↑/↓ 选择，Enter 打开，d 删除。

## 回放 / 分享

- `-replay <id>`：在终端打印该会话的 Trajectory 文本（用户块 / 助手 / 工具行 + diff）后退出，不依赖 LLM。
- `-replay <id> -w`：打印 Web 只读分享链接 `https://<host>/#/share/<id>`，对方在浏览器打开即可只读查看（对话 + 轨迹）。

::: warning 分享链接域名
`-replay -w` 默认打印的 `<host>` 来自 Web UI 自托管的访问地址，需用 `-web-host` 显式传入。文档站的 `gsh.zgric.top` 与 Web UI 自托管域名应分开（例如 Web UI 用 `gsh-app.zgric.top` 子域），避免端口/路径冲突。
:::

## 只读浏览

- `-view <id>` 或会话内 `/readonly`：进入只读模式，可滚动、可切会话，但禁止发送。

## 导出

- `/export` 或 `Ctrl+E`：把当前会话导出为 Markdown 文件（`gsh-session-<id>.md`），落盘到当前工作目录，便于归档或分享。
