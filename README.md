# gsh

DSH（DeepSeek Harness）思想的 Go 重写：**一切皆插件** 的 agent harness，零第三方依赖、单二进制。

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
./gsh.exe -tools                                    # 看当前用哪个 shell、启用了哪些工具、装了哪些技能
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

## 安全策略（对齐 DSH 删盘事故）

bash 工具默认 **安全优先**，这是 DSH 的真实教训——它一条命令清空了用户 D 盘：

- **致命命令永远拦截**（无论配置）：`rm -rf /`、根目录/`--no-preserve-root`、格式化/分区、`dd` 写块设备、fork bomb、关机重启、重定向覆写块设备、递归 `chmod/chown /`、清空根目录文件。
- **受限命令默认拦截**（设 `allow_destructive: true` 放行）：递归删除 `rm -r`/`rmdir -s`/`del /s`、破坏性 `git`（clean/reset --hard/checkout .）、下载脚本管道执行（`curl|sh`）、递归改权限 `chmod -R`、`find -delete`、`truncate -s 0`。
- 配置（`plugins_cfg.tools`）：`allow_destructive`（默认 false）、`workspace`（命令执行目录，默认 `.`）。

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

`./gsh.exe -web` 启动（前端 embed 进二进制，零外部静态文件）：

- 暗色单页：会话侧栏 + 消息流 + 玻璃质感工具调用卡片（可折叠看 args/result）
- SSE 实时流：`tool_call → tool_result → delta(逐字) → final → done`
- API：`POST /api/chat`（SSE）、`GET /api/sessions`、`GET /api/session?id=`
- loop 的 `RunTraced`：工具调用/结果/最终回复以事件回调外露（DSH Trajectory 思想）

## Roadmap

- [x] Web UI 插件（对应 DSH web 模式）
- [x] 危险命令拦截（安全优先，对齐 DSH 删盘事故）
- [x] provider 显式方言（杜绝 hostname 猜测导致 token 翻三倍）
- [x] 内核抽成可 import 的 `harness` 包（gsh 即组件，可嵌入）
- [ ] session resume / fork 的 CLI 入口（`-s <id>` / `--fork`）
- [ ] 事件流按会话回放（Trajectory 视图增强）
- [x] remote/sandbox 工具插件：bash 支持 `node` 参数，经 SSH 分发到 139/82/160 等节点
- [x] remote 支持外部命令后端（`exec`/`args`）：已有远程 shell 工具（如 rzsh）直接接成执行后端
- [x] shell 选择配置化（候选链 / 严格显式 / 失败报错 / 非 POSIX 方言提示 + GBK 转码）
- [x] 工具集扩展（15 个）+ 白名单裁剪 + skills 按需加载
- [ ] `delete_path` 之外的写操作统一审计日志（谁在什么时候动了什么）
- [ ] 工具的 workspace 白名单（限制只能操作某个根目录）
