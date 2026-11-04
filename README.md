# gsh

DSH（DeepSeek Harness）思想的 Go 重写：**一切皆插件** 的 agent harness。核心内核零标准库依赖、单二进制；终端 TUI 前端基于 bubbletea（charmbracelet 生态，静态链接进同一个单二进制，仍是单文件分发、可自托管）。

## 为什么重写

| DSH (TypeScript) | gsh (Go) |
|---|---|
| Cordis 内核（apply/disposer/服务/事件） | `internal/kernel`：同款 Plugin 接口 + 服务注册表 + 事件总线 |
| bundle/profile + cordis.patch.yml | 单个 `config.json` |
| append-only session log、resume/fork | JSONL 会话文件，`List/Open/Fork` |
| Standard/Minimal 运行模式 | `provider: echo`（离线自检）/ `openai`（真跑） |
| pnpm monorepo + Node 依赖 | 纯标准库，`go build` 出单文件 exe |

> 实测对照（同机 Windows）：DSH `npx` 冷启动 **314s**、Web 启动 ~139s 且返回 401、常驻 **310MB**、npx 缓存 **1GB/73k 文件**；gsh 冷启动 **~0.14s**、Web 启动 **~0.36s**、常驻 **~12MB**、产物 **12MB/1 文件**。
> DSH 官方推荐 `npx` 启动本身就是坑：每次启动都要解析 86 个直接依赖的包树，不是模型加载。

## 用法

```bash
go build -o gsh.exe .

# 离线自检（默认 provider=echo，不联网）
./gsh.exe -e "你好"

# 离线自检也能指定真实命令与节点（node/cmd 指令，仅 echo provider 生效）
./gsh.exe -e "node=rzsh cmd=uptime"

# 接 DeepSeek / 任意 OpenAI 兼容端点（new-api 也可）
cp config.example.json config.json   # 填 api_key
./gsh.exe -e "在 rzsh 节点上跑 hostname"   # 模型自主选 node
./gsh.exe -e "数一下 internal 下有几个 Go 文件"   # headless
./gsh.exe                                          # REPL
./gsh.exe -web                                      # Web UI → http://localhost:8934
./gsh.exe -tui                                      # 终端 UI（bubbletea）
./gsh.exe -tools                                    # 看当前用哪个 shell、启用了哪些工具、装了哪些技能
# 会话管理
./gsh.exe -ls                                       # 列出全部会话（id / 时间 / 条数 / 标题）
./gsh.exe -s <id>                                   # 恢复某会话并进入终端 UI（回放历史 + 继续提问）
./gsh.exe --fork <id>                               # 以某会话为蓝本派生新会话后进入
./gsh.exe -replay <id>                              # 非交互回放某会话的事件流（Trajectory 视图）
./gsh.exe -view <id>                               # 只读浏览某会话（终端 UI，禁发送，可 /readonly 切回可编辑）
```

`config.json` 含 API key，已在 `.gitignore` 中排除。

## 执行环境：优先级是配置，不是代码

「用哪个 shell」是环境事实，不该硬编码在程序里。`plugins_cfg.tools.shell` 支持四种写法：

| 写法 | 含义 |
|---|---|
| `"auto"` / 省略 | 平台默认链：Windows `bash → sh → cmd`，其他 `bash → sh` |
| `"bash"` | 单元素链。**显式指定就严格生效**：找不到直接报错，不偷偷换方言 |
| `["bash","sh","cmd"]` | 按序探测，取第一个存在的 |
| `{"bin":"bash","args":["-c"]}` | 精确指定可执行文件与参数（绝对路径也行） |

要点：

- **不静默退化**。显式配了 `bash` 而这台机器没有，命令会返回
  `找不到可用的 shell（已尝试：bash）`，而不是悄悄换成 `cmd` 跑出一半失败的结果。
- 候选链全部不可用时，错误里列出**试过哪些**，让你知道该配什么。
- 真的只能在 `cmd` 下跑时，结果会附 `[hint] 当前 shell 是 cmd…`，让模型自己改写语法。
- Windows 上 `cmd` 的输出是 GBK 字节，gsh 会按系统 ANSI 代码页转 UTF-8（调 kernel32，**不引三方依赖**），中文不再变乱码。

`./gsh.exe -tools` 可以看到解析结果，例如：

```
shell 候选链: bash -c → sh -c → cmd /c
shell 解析: C:\Program Files\Git\usr\bin\bash.exe -c
工具 (15): bash, copy_path, delete_path, edit_file, glob, grep, http_request, list_dir, ...
技能 (1): remote-ops
```

### 作为组件嵌入（重写 DSH 的真正的目的）

gsh 不是只能跑 CLI —— 内核是 Go 包，任何项目三行长出 agent 能力：

```go
import "gsh/harness"

h := harness.New(cfg)    // cfg: *kernel.Config（config.json 结构）
h.LoadDefaults()         // 装载默认插件集
h.Serve(":8080")         // 或 h.Run(ctx, prompt, onDelta) / h.RunREPL(ctx)
defer h.Shutdown()
```

zcbot、AI 沙箱、运维脚本都能这样把 agent 能力塞进去，而不必拖一个 Node 运行时。

## 架构

```
harness/  高层入口：New/LoadDefaults/Serve/Run/RunREPL/RunHeadless（gsh 即组件）
internal/
  kernel/   微内核：Plugin(Apply/Name) + Ctx(Provide/Use/Emit/On/DataDir) + 事件
  api/      跨插件接口：Session/SessionStore/LLM/Loop/ToolRegistry/Tool/Event
  session/  会话插件：JSONL append-only + resume/fork
  tools/    工具插件：15 个内建工具 + 安全策略 + 远程节点 + 技能加载
  llm/      模型插件：echo mock | OpenAI 兼容（显式方言，SSE 流式 + tool_calls 聚合）
  loop/     agent 主循环：模型 ↔ 工具，最大 24 轮保护，RunTraced 事件流
  ui/       REPL + headless(-e)
  web/      Web UI 插件（embed 前端 + SSE + 会话 API）
  tui/      TUI 插件（bubbletea）：终端界面，复用 loop 的流式事件
```

插件装载序固定：`session → tools → llm → loop → web → ui`。
工具结果错误以 `error:` 前缀回填给模型；输出超 60KB 截断。

## 工具集与技能：范围靠配置扩展

工具不是写死的六个。当前内建 15 个，分四类：

| 类别 | 工具 |
|---|---|
| 执行 | `bash`（本地 / 远程节点） |
| 文件读 | `read_file` `list_dir` `glob` `grep` |
| 文件写 | `write_file` `edit_file` `make_dir` `move_path` `copy_path` `delete_path` |
| 网络 | `web_fetch`（HTML 转文本） `http_request`（任意方法/头/体，JSON 自动美化） |
| 技能 | `list_skills` `use_skill`（有技能时才注册） |

**裁剪**（`plugins_cfg.tools`）：

```json
"enabled": ["bash", "read_file", "list_skills", "use_skill"],  // 非空即白名单
"disabled": ["web_fetch"]                                      // 永远排除
```

配了不存在的名字（拼错）会在启动时警告并列出全部可用工具，不静默忽略。

### 技能（skills）

技能是**按需加载的操作手册**，不是知识库：放进来就有，不改代码。

```
skills/
  remote-ops/
    SKILL.md        # --- name/description --- 加正文
```

```json
"skills": { "dir": "./skills" }
```

- `list_skills` 只返回索引（名字 + 一句话描述），**正文不进上下文**。
- 模型判断任务匹配后调 `use_skill` 才加载正文，并拿到技能目录路径（可读附带脚本/参考资料）。
- 这样「技能多」不会挤占上下文——能力范围靠目录扩展，token 按需消耗。

已内置示例 `skills/remote-ops/SKILL.md`：远程节点只读巡检（含判断口径：负载要对核数、分区 >85% 才处理、内存看 available）。

## 安全策略（删盘事故驱动）

bash 工具默认 **安全优先**，这是 DSH 的真实教训——它一条命令清空了用户 D 盘：

- **致命命令永远拦截**（无论配置）：`rm -rf /`、根目录/`--no-preserve-root`、格式化/分区、`dd` 写块设备、fork bomb、关机重启、重定向覆写块设备、递归 `chmod/chown /`、清空根目录文件。
- **受限命令默认拦截**（权限档位切到「完全权限」放行）：递归删除 `rm -r`/`rmdir -s`/`del /s`、破坏性 `git`（clean/reset --hard/checkout .）、下载脚本管道执行（`curl|sh`）、递归改权限 `chmod -R`、`find -delete`、`truncate -s 0`。

### 权限档位（运行期可切换）

读代码和改代码是两种授权，不该逼用户在做只读分析时也交出写权限。三档：

| 档位 | 允许 | 拒绝 |
|---|---|---|
| `read_only` 只读 | `read_file` `glob` `grep` `list_dir` `web_fetch` `http_request` | `bash` 与全部写类工具 |
| `standard` 标准（默认） | 读写文件、常规命令 | 破坏性命令（见上） |
| `full` 完全权限 | 破坏性命令放行 | 灾难性命令仍拦截 |

配置 `plugins_cfg.tools.permission: read_only|standard|full`；Web 输入区的权限按钮可随时切换，**下一次工具调用即生效**。旧字段 `allow_destructive: true` 等价于 `full`。

拦截点在 `registry.Execute` 这个唯一执行入口，不靠每个工具自觉——新增工具漏检不存在。

## 工作区（shell 与文件工具同一坐标系）

切工作区只让 shell 的 `cwd` 变、文件工具仍按进程目录解析，等于给模型两套坐标系：它会读到 A 目录的文件、写进 B 目录，而且都返回成功。

- `plugins_cfg.tools.workspace` 指定初始目录；Web 侧可浏览并切换（`/api/workspace`）。
- `bash` 的 `cmd.Dir` 与 `read_file`/`write_file`/`glob`/`grep`/`list_dir`/`move`/`copy`/`delete` 的路径解析**同源**。
- 相对路径以工作区为基准；**绝对路径不再无条件放行**。

## 路径白名单（workspace 硬边界）

默认开启。`plugins_cfg.tools.workspace_restrict: false` 可关闭（退回旧行为：允许绝对路径出界）。

- 校验点统一在 `Workspace.Resolve`：所有文件类工具都走它，返回 `(path, error)`，越界直接拒绝，不会静默落盘。
- 拦三类逃逸：绝对路径出界、`..` 越界、工作区内符号链接指向外部。判定用 `filepath.Rel`（不是字符串前缀），避免 `app` / `application` 这类兄弟目录被误判为包含。
- 目标文件不存在时逐级上溯到最近的已存在祖先再做 `EvalSymlinks`；含通配符的 glob pattern 也能正确判定。
- 运行期可切换：`POST /api/workspace/restrict {"restrict": bool}`，热生效并写回 config；Web 的「选择工作区」弹窗里有开关。
- **边界（别误当沙箱）**：这层只约束文件工具的路径参数。`bash` 命令本身能 `cd` 出工作区或写绝对路径；HTTP 工具、`skills` 目录加载也不走 `Resolve`。要真正隔离须靠容器 / 系统权限，不是这层字符串校验。

## 远程节点执行（bash 分发到你的节点）

bash 工具支持 `node` 参数，把命令发到配置的节点执行——这是 gsh 在你 139/82/160 多节点基础设施之上的核心用途（本地 agent 变成分布式 agent）。

**两种后端**：

| 后端 | 配置字段 | 说明 |
|---|---|---|
| SSH 直连 | `host` + `user`（+ `port`/`identity`） | 走系统 `ssh` 命令行，命令经 stdin 传给远程 `bash -s` |
| 外部命令 | `exec` + `args` | 把命令交给本地已有可执行程序处理，复用它自己的主机清单/密钥/登录流程 |

配置（`plugins_cfg.tools.remote`）：

```json
"remote": {
  "ssh_bin": "ssh",
  "nodes": {
    "139": {"host": "节点IP或域名", "port": 22, "user": "root", "identity": "/path/to/your_ed25519_key"},
    "rzsh": {"exec": "rzsh", "args": ["{cmd}"]}
  }
}
```

调用（模型填 `node` 参数，或 headless 直接传）：

```json
{"command": "uptime", "node": "139"}
```

`args` 模板占位符：

| 占位符 | 含义 |
|---|---|
| `{cmd}` | 命令原文（引号/空格敏感，简单命令够用） |
| `{cmd64}` | 命令的 base64 编码（推荐，规避所有转义问题） |
| 无占位符 | 命令经 stdin 传入 |

要点：
- SSH 后端走系统命令行（默认 `ssh`，Windows 用 OpenSSH/Git 的 ssh，可在 `ssh_bin` 指定），**零第三方依赖、单二进制不变**。
- 外部命令后端让你已有的远程 shell 工具（如 `rzsh`）直接成为 gsh 的执行后端——不重复造轮子，也不必在 gsh 里重述主机清单和密钥。
- **安全策略对远程同样生效**：致命/受限命令先在本地扫描拦截，**绝不会发到节点**（远程节点更危险，更该拦）。
- `node` 为空或省略 → 本地执行。

实测（`rzsh` 后端 → `kuangxinzero2`，aarch64）：

```
$ ./gsh.exe -e "node=rzsh cmd=uptime"
[tool] bash {"command":"uptime","node":"rzsh"}
[result] 15:18:50 up 1 day, 16:44, 1 user, load average: 0.08, 0.11, 0.04

$ ./gsh.exe -e "node=rzsh cmd=rm -rf /"
[tool] bash {"command":"rm -rf /","node":"rzsh"}
[result] error: 命令被安全策略拦截（致命）：rm -rf 作用于根目录 / 通配 / 家目录（不可逆）
```

## Provider 显式方言（杜绝 token 翻三倍）

DSH 会**按 hostname 猜推理方言**——你在 139 上的 new-api 用中立域名，它按 OpenAI 方言说话，token 悄悄翻三倍。gsh 永不猜：

- `plugins_cfg.llm.base_url` 必须显式填（不靠 hostname 推断）。
- `thinking_format` 显式声明：`none`（默认）/ `deepseek`（reasoner 模型吐 `reasoning_content`，gsh 捕获并兜底空回复）。

## Web UI

`./gsh.exe -web` 启动（前端 embed 进二进制，零外部静态文件），界面按 DSH 1:1 组织：

- **左侧栏**：品牌 / 新会话（`Ctrl+Alt+N`）/ 导航 / 工作区会话树（搜索、按时间筛选、右键重命名/删除、可折叠成图标栏）
- **中栏**：标题栏 + 标准模式徽章 +「对话 / 轨迹」页签；消息流含用户气泡、markdown（标题/列表/表格/代码块复制/链接）、工具调用行（可展开看参数与结果）、状态行（完成/停止/失败）
- **输入区**：附件上传（文本 ≤256KB，内容进上下文，回显抽成卡片）、权限三档切换、模型设置、发送/停止（`Esc` 中断）、底部统计
- **右侧抽屉**：能力与环境——工具规格、技能列表、运行诊断（shell 候选链/解析结果/工作区/权限）
- SSE 实时流：`tool_call → tool_result → delta(逐字) → final → done`
- API：`POST /api/chat`（SSE）、`GET /api/state`、`GET /api/sessions`、`GET /api/session?id=`、`POST /api/session/{new,delete,rename}`、`POST /api/settings`（热更新并写回 config）、`GET/POST /api/permission`、`GET /api/workspace/browse`、`POST /api/workspace`、`POST /api/workspace/restrict`、`GET /api/tools`
- loop 的 `RunTraced`：工具调用/结果/最终回复以事件回调外露，便于上层（TUI / Web / REPL）做轨迹回放

## 终端 UI（TUI）

`./gsh.exe -tui` 启动终端界面（bubbletea 实现；布局与配色按实际截图落地，不是凭印象的「暗色 + 绿色」）。

- 配色（截图采样）：画布 `#0a0a0a`、元素 `#141414`、输入框 `#1e1e1e`、正文 `#e8e8e8`、次级 `#909090`、边框 `#3a3a3a`、强调蓝 `#5898f0`
- **顶部**：圆角标题条，`# 会话标题` 左；右栏空闲时显示 `N 条 · sessionxxxx`，运行中显示 `运行中 mm:ss · sessionxxxx`，跑完显示 `mm:ss · N 条 · sessionxxxx`（不编 token/cost，避免假数字）
- **消息区**：用户消息为「左侧蓝色竖条 + 浅色底块」（不是行内气泡）；助手为普通段落；工具行为 `* Tool args (n)`，读类工具用 `→ Read path`（无 ✓/✗ 图标）；写类工具（write/edit）的结果内嵌**统一 diff**，按 `+/-/块头` 着色
- **底栏**：`◆ 模式 · 模型` 徽标 → 带蓝色左边条的圆角输入框 → `模式 模型 provider` 行 → 键位提示条（`enter 发送` / `shift+enter 换行` / `ctrl+c 退出`，运行中显示 `esc 停止`）
- **交互**：
  - Enter 发送、Shift+Enter 换行、Esc 停止当前轮 / 关闭浮层、Ctrl+C 退出
  - `/help` 帮助浮层、`/sessions`（或 `Ctrl+P`）会话列表（↑/↓、j/k 选择，enter 打开，d 删除）、`/new`、`/clear` 开新会话或清空、`/readonly` 在「只读浏览 ⇄ 可编辑继续」间切换
  - **交互式权限确认**（标准档位下）：bash/write/edit/make_dir/move/copy/delete 等高风险工具执行前弹出 `确认 <工具> → <参数>` 浮层，按 `a` 允许 / `A` 总是允许（本进程后续同类不再询问）/ `d` 或 `esc` 拒绝；只读档位直接拦截、完全档位不询问
- **只读浏览模式**：`-view <id>` 以只读方式进终端 UI 打开会话（顶栏标 `只读浏览`，底栏提示只读、禁发送，Ctrl+P 仍可切会话）；TUI 内随时 `/readonly` 切回可编辑继续
- 复用内核的 `loop.RunSession`：流式 delta 与 `tool_call/tool_result` 事件经 channel 回流，逐字流与工具行实时更新——TUI 只是 loop 的新展示面，与 Web UI、REPL 对等
- **助手回复按 markdown 渲染**（`internal/tui/markdown.go`，自实现行级状态机，不引第三方 markdown 库）：
  - 块级：ATX 标题（`#`~`###`，去标记 + 分级加粗）、无序列表（统一 `•`）、有序列表（保留编号）、引用（左侧竖条 + 次级色）、分割线、围栏代码块（左竖条 + 浅底块 + 语言标签）
  - 行内：`` `code` ``（浅底色块）、`**bold**`/`__bold__`、`*italic*`/`_italic_`、`[text](url)`（蓝色下划线 + 灰色 URL）
  - 折行按**终端单元格宽度**计算（全角字符占 2 列），中文无空格长段落逐字断行，不会溢出右边界
  - `_` 只在词边界才当强调，不会把 `tool_name_here`、`my_file.go` 误渲染成斜体
  - 未闭合的代码围栏不丢内容（按代码块兜底输出）
- 已知差距：表格与嵌套强调（如粗体里套行内码）未支持，会退化成纯文本


## 操作审计日志

gsh 作为未来嵌进沙箱 / 运维体系的 harness，操作可追溯是刚需。`registry.Execute`（工具唯一执行入口）每次调用都会记一条 append-only 审计记录，落 `<data_dir>/tools/audit/audit.log`（JSONL）：

- 三种动作：`denied`（工具不存在）/ `blocked`（被权限闸门拦）/ `execute`（真实执行）
- 字段：时间戳、会话 id、工具名、**脱敏后的参数**、远程节点、权限档位、成功与否、错误摘要、结果长度（不记内容）、耗时
- **参数脱敏**：敏感键名（api_key/token/secret/password…）整体掩码；命令/值里偶发的 `sk-…`/`Bearer …`/`key=…` 也掩码。**绝不记工具输出原文或文件内容**
- 默认开启；`plugins_cfg.tools.audit: false` 关闭
- 查询：`GET /api/audit?n=200`（最多 1000）；`Recent(n)` 读尾 n 行
- **Web 展示面板**：侧栏「审计日志」进入独立视图，按 `全部 / 执行 / 拒绝 / 拦截` 筛选，显示动作徽标、工具、时间、成功/失败、会话、节点、权限、耗时、输出大小、拦截原因/错误，以及脱敏后的参数原文；打开后每 4s 轻量轮询顶部新记录，手动刷新按钮随时拉取最新

## Roadmap

- [x] Web UI 插件（对应 DSH web 模式）
- [x] 危险命令拦截（安全优先，删盘事故驱动）
- [x] provider 显式方言（杜绝 hostname 猜测导致 token 翻三倍）
- [x] 内核抽成可 import 的 `harness` 包（gsh 即组件，可嵌入）
- [x] session resume / fork 的 CLI 入口（`-s <id>` / `--fork` / `-ls` / `-replay`）
- [x] 事件流按会话回放（Trajectory 视图，非交互复盘）
- [x] remote/sandbox 工具插件：bash 支持 `node` 参数，经 SSH 分发到 139/82/160 等节点
- [x] remote 支持外部命令后端（`exec`/`args`）：已有远程 shell 工具（如 rzsh）直接接成执行后端
- [x] shell 选择配置化（候选链 / 严格显式 / 失败报错 / 非 POSIX 方言提示 + GBK 转码）
- [x] 工具集扩展（15 个）+ 白名单裁剪 + skills 按需加载
- [x] 权限三档（read_only/standard/full），运行期切换，registry 统一拦截
- [x] 统一工作区：shell cwd 与文件工具路径解析同源
- [x] Web UI 1:1（会话树/轨迹/附件/权限/设置/能力面板/重命名/搜索/筛选）
- [x] 终端 TUI（bubbletea，复用 loop 流式事件）
- [x] TUI markdown 渲染（标题/列表/引用/代码块/行内码/链接，按终端单元格宽度折行）
- [x] TUI 写类工具内嵌统一 diff（着色渲染）
- [x] TUI 斜杠命令 + 帮助浮层（`/help` `/sessions` `/new` `/clear`）
- [x] TUI 会话列表 `Ctrl+P`（切换 / 删除 / 回放）
- [x] TUI 顶栏实时计时与消息计数
- [x] TUI 交互式权限确认（标准档位高风险工具 allow / always / deny）
- [x] 操作审计日志（落盘 + 脱敏 + 查询 API + Web 展示面板）
- [x] 工具的 workspace 白名单（Resolve 硬边界：绝对路径 / `..` / 符号链接越界，运行期开关）

## License

[MIT](LICENSE) © 2026 kuangxing6367

第三方依赖各自沿用其原协议（charmbracelet 系为 MIT；`golang.org/x/*` 为 BSD-3-Clause）。内核本身只用 Go 标准库。

