# 架构

gsh 是微内核 + 插件化结构，分层如下：

| 层 | 职责 |
| --- | --- |
| `kernel` | 微内核：插件加载、依赖注入（`Use[T]`）、配置与作用域生命周期。 |
| `api` | 类型化服务接口（LLM / Tool / ToolRegistry / SessionStore / Loop / Event）。插件只依赖此层，互不直接 import。 |
| `loop` | Agent 主循环：把用户输入跑成流式事件（delta / tool_call / tool_result），通过 channel 回流。 |
| `session` | append-only JSONL 会话存储：resume / fork / 删除，标题取首条用户消息摘要。 |
| `tools` | 工具注册表 + 权限闸门（guard）+ 审计日志（audit）+ 审批器（approver）。写类工具内嵌统一 diff。 |
| `tui` / `web` | 两个对等的展示面，都消费 loop 的流式事件，不引入新 agent 逻辑。 |

## 事件回流

loop 在独立 goroutine 顺序执行工具调用；TUI 用 channel 把事件桥接回 bubbletea 的 Update，实现逐字流与工具行实时状态。Web UI 走同样的事件模型，只是渲染到浏览器 DOM。
