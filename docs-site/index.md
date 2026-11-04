---
layout: home

hero:
  name: gsh
  text: 微内核 · 插件化 · 终端式 AI Agent
  tagline: 一个内核同时驱动终端 UI、Web UI 与 CLI，三者完全对等；会话以 append-only JSONL 落盘，可恢复、可分叉、可回放、可只读分享。
  actions:
    - theme: brand
      text: 快速开始
      link: /guide/quickstart
    - theme: alt
      text: 在 GitHub 查看
      link: https://github.com/kuangxing6367/gsh

features:
  - title: 一个内核，多种界面
    details: 同一套 agent loop 与工具执行，既能在终端用 bubbletea 交互，也能在浏览器看轨迹，也能用命令行一次性跑完。
  - title: 会话是第一公民
    details: 每次运行都落盘成可回放的事件流，不怕中断，可事后复盘或分享给他人只读查看。
  - title: 安全默认
    details: 标准档位对高风险工具做交互式确认；工作区路径白名单阻止越界读写；删除即真删。
---

## 能力一览

- **微内核**：插件加载、依赖注入、配置与作用域生命周期由内核统一管理。
- **插件化**：LLM / 工具 / 会话 / 展示面都是插件，互不直接 import，只依赖类型化服务接口。
- **TUI / Web / CLI 三端对等**：终端 UI、Web UI、命令行共用同一套 loop，只是展示层不同。
- **会话回放**：事件流可恢复、可分叉、可非交互回放。
- **只读分享**：`-replay -w` 生成 Web 只读链接，对方在浏览器即可查看轨迹。
- **交互式权限**：标准档位下高风险工具执行前弹确认，支持「总是允许」白名单。
- **工作区白名单**：文件类工具越界（绝对路径、`..`、符号链接逃逸）被拒绝。
