# 快速开始

## 环境要求

- Go 1.22 及以上
- 一个兼容 OpenAI 协议的 LLM（在 `config.json` 里配置 `base_url` / `api_key` / `model`）

## 构建与运行

```bash
# 克隆后进入仓库
go build -o gsh .

# 启动终端 UI（推荐）
./gsh -tui

# 或浏览器访问 Web UI
./gsh -web

# 或一次性跑完（headless）
./gsh -e "帮我列出当前目录并说明每个文件作用"
```

## 最小配置

把 LLM 连接信息写进 `config.json`（与可执行文件同目录）：

```json
{
  "providers": {
    "openai": { "base_url": "https://api.openai.com/v1", "api_key": "sk-..." }
  },
  "llm": { "provider": "openai", "model": "gpt-4o-mini" },
  "tools": { "permission": "standard", "workspace_restrict": true },
  "plugins": ["session", "tools", "loop", "tui", "web", "repl"]
}
```
