# 开发

## 构建与测试

```bash
go build ./...
go vet ./...
go test ./...

# 本地跑 TUI / Web 前先编译
go build -o gsh . && ./gsh -tui
```

## 目录结构

```text
gsh/
  main.go              CLI 入口与 flag 路由
  harness/             组装内核、提供 RunTUI / RunREPL / Serve 等
  internal/
    api/               类型化服务接口
    kernel/            微内核 + 插件加载 + 依赖注入
    loop/              Agent 主循环 + 流式事件
    session/           append-only 会话存储
    tools/             工具注册表 + guard + audit + approver + diff
    tui/               bubbletea 终端 UI
    web/               Web UI（静态前端 + Go 服务端）
    ui/                非交互展示：回放 / Markdown 导出
  docs-site/           本文档站（VitePress），部署到 GitHub Pages
```

## 文档站部署

本站点（`docs-site/`）是 VitePress 项目，构建产物通过 GitHub Pages 发布到 `gsh.zgric.top`（自定义域名）。推送 `main` 后由 `.github/workflows/deploy.yml` 自动构建并部署到 `gh-pages` 分支。

本地预览文档站：

```bash
cd docs-site
npm install
npm run dev
```

手动构建产物：

```bash
npm run build   # 输出到 docs-site/.vitepress/dist
```
