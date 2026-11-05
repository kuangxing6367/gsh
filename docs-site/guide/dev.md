# 开发

## 构建与测试

```bash
go build ./...
go vet ./...
go test ./...

# 本地跑 TUI / Web 前先编译
go build -o gsh . && ./gsh -tui
```

## 打包与二进制兼容

```bash
scripts/build-release.sh 0.2.0   # 8 个平台，输出到 dist/
```

产物是静态链接的 Go 二进制，不依赖 glibc。打包时会逐个校验 Linux 产物：含 `PT_INTERP`、有 `DT_NEEDED`、或引用了 `GLIBC_x.y` 版本，直接构建失败。

必须校验的原因：CI 在 ubuntu runner 上编 `linux/amd64` 属于同平台编译，CGO 默认为 1。漏掉 `CGO_ENABLED=0` 就会绑死 runner 的 glibc（比如 2.39），产物在 CentOS 7 上直接报 `version 'GLIBC_2.34' not found`——开发机上完全看不出来。

真正跑不动的边界在别处：

| 限制 | 边界 |
|---|---|
| 内核版本 | Linux ≥ 3.2（loong64 需 ≥ 5.19） |
| amd64 指令集 | `GOAMD64=v1`，所有 64 位 x86 |
| arm64 指令集 | `GOARM64=v8.0` |
| arm 指令集 | `GOARM=7`，需 VFPv3（Cortex-A）；ARMv5 / ARM11 不行 |

Alpine、distroless 这类无 glibc 镜像可以直接跑。CGO 关闭时 `os/user` 只读 `/etc/passwd` 和 `/etc/group`，靠 NSS 查 LDAP / SSSD 的用户名解析不可用。

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
