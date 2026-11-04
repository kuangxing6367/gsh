# 配置

`config.json` 声明 LLM 连接、权限策略与启用的插件：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `providers` | 对象 | LLM 供应商集合，每项含 `base_url` 与 `api_key`。 |
| `llm.provider` | 字符串 | 选用哪个 provider 的键名。 |
| `llm.model` | 字符串 | 模型名。 |
| `tools.permission` | `read_only` / `standard` / `full` | 权限档位：只读拦截写工具；标准档位对高风险工具做交互式确认；完全档位放行。 |
| `tools.workspace_restrict` | bool | 工作区路径白名单开关。开启后工具只能在会话/工作区内读写，越界（绝对路径、`..`、符号链接逃逸）被拒绝。 |
| `session.dir` | 字符串 | 会话 JSONL 存放目录（默认内核 DataDir 下的 `sessions/`）。 |
| `plugins` | 字符串数组 | 启用的插件名列表，如 `session` / `tools` / `loop` / `tui` / `web` / `repl`。 |

## 权限三档

- `read_only`：guard 直接拦截所有写工具，最安全。
- `standard`（默认）：读工具直放，写工具执行前交互式确认。
- `full`：全部放行，不做确认——仅在你完全信任输入时用。

## 工作区白名单

开启 `workspace_restrict` 后，文件类工具会做两层检查（静态路径相对计算 + 解析符号链接后的祖先校验），阻止通过 `../` 或符号链接逃逸出工作区。运行期可经 Web API 开关。
