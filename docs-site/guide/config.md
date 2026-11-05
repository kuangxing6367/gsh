# 配置

`config.json` 放在**可执行文件同目录**。首次运行若不存在会自动生成一份最小可用模板（权限 `0600`，因为里面含 API key）。配置文件、数据目录、日志、技能目录都以程序目录为锚点，从任何当前工作目录启动行为一致。

```
<gsh.exe 所在目录>/
  config.json          连接、权限、模型、提示词、会话策略
  .gsh/
    session/sessions/  会话 JSONL（append-only）
    tools/audit/       操作审计日志
    logs/              运行日志（按天切分）
  AGENTS.md            可选：项目级约定，每轮自动注入系统提示
  skills/              可选：按需加载的操作手册
```

顶层结构：

| 字段 | 说明 |
| --- | --- |
| `data_dir` | 数据根目录，相对路径按程序目录解析 |
| `plugins` | 装载的插件名（缺省时用内置默认集） |
| `plugins_cfg` | 每个插件的配置节，见下表 |

## llm：模型与推理

**单模型写法**（`config.example.json` 的默认形态）把所有字段平铺在 `llm` 下：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `provider` | `openai` | `openai`（任意 OpenAI 兼容端点）或 `echo`（离线自检） |
| `base_url` | `https://api.deepseek.com` | 不带 `/chat/completions` |
| `model` | `deepseek-chat` | 模型名 |
| `api_key` | 空 | 支持 `env:变量名`，运行时从环境变量读取，不落明文 |
| `thinking_format` | `none` | `deepseek` 表示对端会吐推理字段；只影响解析，不参与请求注入 |
| `timeout_seconds` | `120` | 单次请求整体超时 |
| `params` | — | `temperature` / `top_p` / `max_tokens` / `presence_penalty` / `frequency_penalty` / `stop`。未配置就不发该字段，由对端决定默认值 |
| `thinking` | 关闭 | `enabled` / `style`（`auto`\|`openai`\|`anthropic`\|`flag`）/ `effort` / `budget_tokens`。不同网关认的字段不同，不靠猜 |
| `retry` | `{max:2, base_ms:800}` | 只对 429 / 5xx / 网络错误指数退避重试，4xx 参数错误不重试 |

**多模型写法**用 `models` 表，顶层的 `provider` / `base_url` / `api_key` / `timeout_seconds` / `retry` / `params` 作为公共缺省，子项只写要覆盖的部分：

```json
"llm": {
  "active": "main",
  "aux": "cheap",
  "models": {
    "main":  {"model": "deepseek-chat",     "api_key": "env:GSH_MAIN_KEY"},
    "cheap": {"model": "deepseek-chat-mini", "api_key": "env:GSH_AUX_KEY"}
  }
}
```

- `active` 是主模型，跑对话；`aux` 是辅助模型，跑上下文摘要。没配 `aux` 时退回主模型。
- 切换入口：`gsh -model <名字>`（本次进程）、TUI `/model <名字>` 与 `/aux <名字>`、`GET /api/models` 与 `POST /api/models/use`。切换在下一轮请求生效，不打断正在进行的补全。
- 推理内容（`reasoning_content` / `reasoning` / `thinking` / `reasoning_delta`）走独立事件通道展示，**不写进会话历史**，避免污染上下文与重复计费。

## prompt：系统提示

每轮请求都会注入一条 system 消息，按固定顺序拼装：

1. 内置三节：角色与工作方式 / 沟通规范 / 运行环境（工作区、系统、权限档位、时间、工具清单）
2. 工作区根目录的 `AGENTS.md`（或 `GSH.md` / `agents.md`），超 32KB 截断
3. `prompt.active` 选中的人格补充
4. `prompt.system` 自定义指令

```json
"prompt": {
  "active": "review",
  "personas": { "review": "只做代码审查，不改文件。" },
  "system": "先给结论。"
}
```

工作区切换后，系统提示里的路径与项目约定会实时重读，不会停留在旧目录。

## session：上下文压缩

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `compact_enabled` | `true` | 关掉后完全不压缩 |
| `max_chars` | `24000` | 历史总字符数超此值触发 |
| `keep_turns` | `20` | 压缩后保留最近多少轮 |
| `hard_cap` | `400` | 消息条数硬上限 |
| `max_tokens` | `0` | 显式 token 阈值；0 表示由模型窗口推导 |
| `token_ratio` | `0.75` | 占上下文窗口的比例上限，超过即压缩 |
| `dir` | `.gsh/session/sessions` | 会话目录，相对路径按程序目录解析 |

token 判定优先于字符判定：模型配置了 `llm.context_window` 后按窗口比例触发；窗口未知时自动退回 `max_chars`。

token 数是估算的，不是精确值。估算口径：ASCII 约 4 字符/token，CJK 按 1 字符/token（偏保守，宁可早压缩也不要撞窗口）；工具定义本身也计入。只要 provider 在响应里上报了真实 `prompt_tokens`，估算系数就会按 `实际 / 估算` 自动校准（滑动平均，单轮权重 30%）——不同 tokenizer 对中文的切分差异很大，写死常数在某些模型上必然偏。provider 未上报（如本地 `echo`）时不做校准，避免用自己的估算校准自己。

压缩按**轮边界**切分（切点只落在 user 消息上），保证 assistant 的 `tool_calls` 与其工具结果不被拆散。早期对话交给辅助模型生成摘要；摘要失败自动降级成纯裁剪，不阻断会话。压缩后的摘要会合并进主系统提示，因此出站请求始终只有一条 system 消息。

## tools

| 字段 | 说明 |
| --- | --- |
| `permission` | `read_only` / `standard` / `full` / `plan`，运行期可切换 |
| `workspace` | 初始工作区，相对路径按程序目录解析 |
| `workspace_restrict` | 路径白名单开关，默认开启 |
| `tool_timeout_seconds` | 单次工具执行超时，默认 60 秒；卡住的命令不会拖死整轮对话 |
| `shell` | `auto` / 单个 shell / 候选数组 / `{bin,args}` 精确指定 |
| `enabled` / `disabled` | 工具白名单与排除项 |
| `skills.dir` | 技能目录 |
| `remote` | 远程节点配置 |
| `audit` | 操作审计开关，默认开启 |
| `parallel_tools` | 并行执行同一轮的多个工具调用，默认开启 |
| `parallel_limit` | 并行上限，默认 4 |
| `websearch` | 联网检索配置，见下 |

存在交互式审批器时（TUI）工具自动串行执行——多个确认弹窗会抢同一路输入。

只读档与计划模式靠**工具自己声明的 `ReadOnly`** 判断能不能放行：没声明的工具一律按「可能写」处理。这样新增的工具、以及 MCP 接进来的工具都走默认拒绝——漏声明的后果是「用不了」，而不是「能写」。`todo_write`、`remember` 属于 agent 自身状态记录（固定写在 `.gsh` 下），不碰工作区，因此在只读档里也放行。

`websearch` 节：

| 字段 | 说明 |
| --- | --- |
| `provider` | `duckduckgo`（默认，无需密钥）/ `searxng` / `tavily` |
| `proxy` | 检索请求走的代理，例如 `http://127.0.0.1:7892`；不填则直连 |
| `endpoint` | `searxng` 实例地址；不填则该项不可用 |
| `api_key` | `tavily` 必填，支持 `env:` 前缀 |
| `max_results` | 单次返回条数，默认 5 |

未配置 `websearch` 节时不注册该工具——给模型一个注定失败的工具比不给更糟。

## memory：长期记忆

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 关闭后不注入、不注册 `remember` 工具 |
| `path` | `.gsh/memory/MEMORY.md` | 记忆文件，相对路径按程序目录解析 |

记忆文件每一行是一条带日期的记录，由 `remember` 工具追加。注入系统提示时最多取最近 4000 字符，文件本身不裁剪。它与计划清单（`todo_write`）的区别：清单记本轮进度、用完即弃；记忆记跨会话的事实。

## hooks：外部钩子

工具执行前后各有一个插口，判定权交给外部命令而不是内置规则：

```json
"hooks": {
  "before_tool": [
    { "match": "^bash$", "command": "grep -q 'rm -rf' <<< \"$GSH_ARGS\" && exit 1", "timeout_seconds": 5 }
  ],
  "after_tool": [
    { "match": "^edit_file$", "command": "gofmt -l \"$GSH_ARGS\"" }
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `match` | 工具名正则，不填表示全部工具 |
| `command` | 要执行的命令；Unix 走 `sh -c`，Windows 走 `cmd /C` |
| `timeout_seconds` | 单条钩子超时，默认 10 秒 |

命令通过环境变量拿到上下文：`GSH_PHASE` / `GSH_TOOL` / `GSH_ARGS` / `GSH_SESSION` / `GSH_RESULT` / `GSH_ERROR`。

`GSH_ARGS` 与 `GSH_RESULT` 超过 4KB 时不再走环境变量，改为写成临时文件（`0600`，命令结束后自动删除），对应变量变成 `GSH_ARGS_FILE` / `GSH_RESULT_FILE`。原因是 Windows 的整个环境块上限只有 32KB：一次 `write_file` 的参数或一段构建日志就能顶爆，子进程会直接起不来。需要完整内容时按 `$GSH_ARGS_FILE` 读文件；只做关键词匹配的简单规则用内联版本就够。

`before_tool` 以**非零码退出即拦截**，拦截原因（stdout+stderr）会作为工具结果回给模型，模型看到的是「为什么被拒绝」而不是凭空少了一条结果。`after_tool` 的非空输出会追加到工具结果末尾，前缀 `[hook]`，适合做格式检查、敏感信息提示这类补注。

补注进入上下文前会截到 4KB：钩子输出直接接在工具结果后面，不设上限的话一条 `cat big.log` 就能让每次调用都多带几十 KB。

钩子是同步阻塞执行的：一条钩子卡住就等于整轮卡住，所以每条都有超时兜底。超时后结束的是**整棵进程树**，不只是 `sh` / `cmd` 本身——否则钩子里起的程序会留成孤儿进程。

## codeindex：代码库索引

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 关闭后不建索引、不注册 `code_search` |

索引只做定位，不含实现：扫工作区内的源码文件，按行抽取函数/类/类型定义（Go、Python、JS/TS、Rust、Java 等），落盘到 `.gsh/codeindex/index.json`（`0600`）。启动时只读缓存、不主动扫盘；工作区变化后重建即可。模型要定位某个符号时先查索引，拿到文件与行号后再 `read_file` 精读，比一路 grep 省得多。

单仓库规模上限：5000 个文件、单文件 512KB。超过这个量级说明该上真正的语言服务器。

## subagent：子 agent 派活

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `max_turns` | `12` | 单个子任务的轮数上限 |

主 agent 通过 `task` 工具把一段独立调研外包出去。子 agent 有自己的上下文与轮数预算，跑完只回一句结论——主会话窗口不会被子任务翻看的文件内容撑爆。

子 agent 默认**只有只读工具**：主 agent 看不见它干了什么，让它在背后改文件等于放弃回滚点。确需写入时，调用 `task` 时把 `allow_write` 设为 `true`。它永远拿不到 `task` 自身，不允许递归派活。

## mcp：接入外部工具

```json
"mcp": {
  "servers": [
    { "name": "fs", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."] }
  ]
}
```

启动时逐个拉起 server，握手后把它的工具并进同一张工具表，名字形如 `mcp_<server>_<tool>`（带前缀是因为不同 server 的工具重名很常见）。支持 `env` 传环境变量。

`servers[]` 可加 `"read_only": true` 声明这个 server 的工具不会改动本地文件。**默认是 false**：MCP 无法从工具描述可靠判断它会做什么，所以只读模式与计划模式下一律拒绝未声明只读的 MCP 工具。想让只读模式里也能用某个检索类 server，就在配置里显式声明。

只实现 stdio + tools 能力：这两样覆盖绝大多数现成 server。单个 server 起不来只跳过它自己，不影响进程启动；原因通过 `mcp.error` 事件上抛。

`servers` 默认空数组——没有明确需求的进程不该在启动时去拉子进程。

## 路径与安全

- 所有相对路径（配置、数据、工作区、技能）统一相对**可执行文件目录**解析，不随 cwd 漂移。
- `config.json`、会话 JSONL、审计日志、运行日志均以 `0600` 落盘。
- `api_key` 可用 `env:` 前缀引用环境变量；日志只记录路径与错误摘要，不记录密钥与工具输出原文。
- 审计参数会自动脱敏（密钥、token、Bearer、password 等）。
- 启动横幅会打印配置文件、数据目录、日志文件的绝对路径；`-v` 可直接查看。

## web

`web.addr` 默认 `:8934`。`-web` 启动本地服务，前端已 embed 进二进制，无外部静态文件。

常用接口：`POST /api/chat`（SSE）、`GET /api/state`、`GET /api/models`、`POST /api/models/use`、`GET/POST /api/session*`、`GET /api/tools`、`GET/POST /api/permission`、`POST /api/settings`、`GET /api/audit`。
