import React, { useState, useEffect } from 'react'

const sections = [
  { id: 'intro', label: '介绍' },
  { id: 'quickstart', label: '快速开始' },
  { id: 'cli', label: 'CLI 用法' },
  { id: 'tui', label: '终端 UI' },
  { id: 'architecture', label: '架构' },
  { id: 'config', label: '配置' },
  { id: 'sessions', label: '会话管理' },
  { id: 'dev', label: '开发' },
]

function Intro() {
  return (
    <div>
      <h1>gsh</h1>
      <p className="lead">
        微内核 + 插件化的终端式 AI Agent。一个内核同时驱动终端 UI、Web UI 与 CLI，
        三者完全对等；会话以 append-only JSONL 落盘，可恢复、可分叉、可回放、可只读分享。
      </p>
      <p style={{ marginTop: 18 }}>
        <span className="badge">微内核</span>
        <span className="badge">插件化</span>
        <span className="badge">TUI / Web / CLI 三端对等</span>
        <span className="badge">会话回放</span>
        <span className="badge">只读分享</span>
        <span className="badge">交互式权限</span>
        <span className="badge">工作区白名单</span>
      </p>
      <h2>为什么是 gsh</h2>
      <ul>
        <li><b>一个内核，多种界面</b>：同一套 agent loop 与工具执行，既能在终端用 bubbletea 交互，也能在浏览器看轨迹，也能用命令行一次性跑完。</li>
        <li><b>会话是第一公民</b>：每次运行都落盘成可回放的事件流，不怕中断，可事后复盘或分享给他人只读查看。</li>
        <li><b>安全默认</b>：标准档位对高风险工具做交互式确认；工作区路径白名单阻止越界读写；删除即真删。</li>
      </ul>
    </div>
  )
}

function QuickStart() {
  return (
    <div>
      <h1>快速开始</h1>
      <h2>环境要求</h2>
      <ul>
        <li>Go 1.22 及以上</li>
        <li>一个兼容 OpenAI 协议的 LLM（在 config.json 里配置 baseUrl / apiKey / model）</li>
      </ul>
      <h2>构建与运行</h2>
      <pre><code>{`# 克隆后进入仓库
go build -o gsh .

# 启动终端 UI（推荐）
./gsh -tui

# 或浏览器访问 Web UI
./gsh -web

# 或一次性跑完（headless）
./gsh -e "帮我列出当前目录并说明每个文件作用"`}</code></pre>
      <h2>最小配置</h2>
      <p>把 LLM 连接信息写进 <code>config.json</code>（与可执行文件同目录）：</p>
      <pre><code>{`{
  "providers": {
    "openai": { "base_url": "https://api.openai.com/v1", "api_key": "sk-..." }
  },
  "llm": { "provider": "openai", "model": "gpt-4o-mini" },
  "tools": { "permission": "standard", "workspace_restrict": true },
  "plugins": ["session", "tools", "loop", "tui", "web", "repl"]
}`}</code></pre>
    </div>
  )
}

function CLI() {
  const rows = [
    ['-tui', '启动终端 UI（默认若未指定其它模式）'],
    ['-web', '启动 Web UI（浏览器访问）'],
    ['-e "<prompt>"', 'headless 模式：发送一条消息并跑完一轮'],
    ['-s <id>', '恢复指定会话并进入终端 UI（回放历史 + 继续）'],
    ['--fork <id>', '以某会话为蓝本派生新会话后进入'],
    ['-view <id>', '以只读浏览模式进入终端 UI'],
    ['-replay <id>', '非交互回放某会话的事件流（Trajectory 视图），打印后退出'],
    ['-replay <id> -w', '打印该会话的 Web 只读分享链接，不输出正文'],
    ['-ls', '列出全部会话（id / 时间 / 条数 / 标题）'],
    ['-tools', '打印当前 shell / 已启用工具 / 技能清单'],
    ['-v', '打印版本与已加载插件'],
    ['-cfg <path>', '指定配置文件路径（默认 ./config.json）'],
  ]
  return (
    <div>
      <h1>CLI 用法</h1>
      <p>所有入口都走同一个可执行文件，用 flag 切换行为：</p>
      <table>
        <thead><tr><th>Flag</th><th>说明</th></tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r[0]}><td><code>{r[0]}</code></td><td>{r[1]}</td></tr>
          ))}
        </tbody>
      </table>
      <p className="muted">示例：<code>{'./gsh -replay 20260101-120000-ab12 -w'}</code> 会输出 <code>https://gsh.zgric.top/#/share/20260101-120000-ab12</code></p>
    </div>
  )
}

function TUI() {
  const keys = [
    ['Enter', '发送（Shift+Enter 换行）'],
    ['Esc', '运行中停止当前轮 / 关闭浮层'],
    ['Ctrl+C', '退出'],
    ['Ctrl+P', '打开会话列表（↑/↓、j/k 选择，Enter 打开，d 删除）'],
    ['Ctrl+E', '把当前会话导出为 Markdown 文件'],
    ['a / A / d', '权限确认浮层中：允许 / 总是允许 / 拒绝'],
  ]
  const cmds = [
    ['/help', '显示帮助浮层'],
    ['/sessions', '打开会话列表（等同 Ctrl+P）'],
    ['/readonly', '在「只读浏览」与「可编辑继续」之间切换'],
    ['/export', '导出当前会话为 Markdown（等同 Ctrl+E）'],
    ['/new', '开新会话'],
    ['/clear', '清空当前会话'],
  ]
  return (
    <div>
      <h1>终端 UI</h1>
      <p>
        终端 UI 用 bubbletea + lipgloss 实现，视觉规范按实际截图落地：
        顶部圆角标题条、左侧蓝色竖条的用户块、等宽工具行、底部带左边条的输入框。
      </p>
      <h2>快捷键</h2>
      <table>
        <thead><tr><th>按键</th><th>作用</th></tr></thead>
        <tbody>
          {keys.map((r) => (<tr key={r[0]}><td><span className="kbd">{r[0]}</span></td><td>{r[1]}</td></tr>))}
        </tbody>
      </table>
      <h2>斜杠命令</h2>
      <table>
        <thead><tr><th>命令</th><th>作用</th></tr></thead>
        <tbody>
          {cmds.map((r) => (<tr key={r[0]}><td><code>{r[0]}</code></td><td>{r[1]}</td></tr>))}
        </tbody>
      </table>
      <h2>交互式权限</h2>
      <p>
        标准档位下，<code>bash</code> / <code>write</code> / <code>edit</code> / <code>make_dir</code> /
        <code>move</code> / <code>copy</code> / <code>delete</code> 等高风险工具执行前会弹出确认浮层：
      </p>
      <ul>
        <li><span className="kbd">a</span> 本次允许</li>
        <li><span className="kbd">A</span> 总是允许（计入本进程白名单，直到退出）</li>
        <li><span className="kbd">d</span> 或 <span className="kbd">Esc</span> 拒绝</li>
      </ul>
      <p className="muted">只读档位由全局 guard 直接拦截写工具，完全档位跳过确认。</p>
      <h2>只读浏览模式</h2>
      <p>
        用 <code>-view &lt;id&gt;</code> 或会话内 <code>/readonly</code> 进入只读浏览：
        可滚动查看历史、可用 Ctrl+P 切换会话，但禁止发送新消息、隐藏输入框。适合复盘或演示。
      </p>
    </div>
  )
}

function Architecture() {
  return (
    <div>
      <h1>架构</h1>
      <p>gsh 是微内核 + 插件化结构，分层如下：</p>
      <table>
        <thead><tr><th>层</th><th>职责</th></tr></thead>
        <tbody>
          <tr><td><code>kernel</code></td><td>微内核：插件加载、依赖注入（<code>Use[T]</code>）、配置与作用域生命周期。</td></tr>
          <tr><td><code>api</code></td><td>类型化服务接口（LLM / Tool / ToolRegistry / SessionStore / Loop / Event）。插件只依赖此层，互不直接 import。</td></tr>
          <tr><td><code>loop</code></td><td>Agent 主循环：把用户输入跑成流式事件（delta / tool_call / tool_result），通过 channel 回流。</td></tr>
          <tr><td><code>session</code></td><td>append-only JSONL 会话存储：resume / fork / 删除，标题取首条用户消息摘要。</td></tr>
          <tr><td><code>tools</code></td><td>工具注册表 + 权限闸门（guard）+ 审计日志（audit）+ 审批器（approver）。写类工具内嵌统一 diff。</td></tr>
          <tr><td><code>tui</code> / <code>web</code></td><td>两个对等的展示面，都消费 loop 的流式事件，不引入新 agent 逻辑。</td></tr>
        </tbody>
      </table>
      <h2>事件回流</h2>
      <p>
        loop 在独立 goroutine 顺序执行工具调用；TUI 用 channel 把事件桥接回 bubbletea 的 Update，
        实现逐字流与工具行实时状态。Web UI 走同样的事件模型，只是渲染到浏览器 DOM。
      </p>
    </div>
  )
}

function Config() {
  const rows = [
    ['providers', '对象', 'LLM 供应商集合，每项含 base_url 与 api_key。'],
    ['llm.provider', '字符串', '选用哪个 provider 的键名。'],
    ['llm.model', '字符串', '模型名。'],
    ['tools.permission', 'read_only / standard / full', '权限档位：只读拦截写工具；标准档位对高风险工具做交互式确认；完全档位放行。'],
    ['tools.workspace_restrict', 'bool', '工作区路径白名单开关。开启后工具只能在会话/工作区内读写，越界（绝对路径、..、符号链接逃逸）被拒绝。'],
    ['session.dir', '字符串', '会话 JSONL 存放目录（默认内核 DataDir 下的 sessions/）。'],
    ['plugins', '字符串数组', '启用的插件名列表，如 session / tools / loop / tui / web / repl。'],
  ]
  return (
    <div>
      <h1>配置</h1>
      <p><code>config.json</code> 声明 LLM 连接、权限策略与启用的插件：</p>
      <table>
        <thead><tr><th>字段</th><th>类型</th><th>说明</th></tr></thead>
        <tbody>
          {rows.map((r) => (<tr key={r[0]}><td><code>{r[0]}</code></td><td>{r[1]}</td><td>{r[2]}</td></tr>))}
        </tbody>
      </table>
      <h2>权限三档</h2>
      <ul>
        <li><code>read_only</code>：guard 直接拦截所有写工具，最安全。</li>
        <li><code>standard</code>（默认）：读工具直放，写工具执行前交互式确认。</li>
        <li><code>full</code>：全部放行，不做确认——仅在你完全信任输入时用。</li>
      </ul>
      <h2>工作区白名单</h2>
      <p>
        开启 <code>workspace_restrict</code> 后，文件类工具会做两层检查（静态路径相对计算 + 解析符号链接后的祖先校验），
        阻止通过 <code>../</code> 或符号链接逃逸出工作区。运行期可经 Web API 开关。
      </p>
    </div>
  )
}

function Sessions() {
  return (
    <div>
      <h1>会话管理</h1>
      <p>会话以 append-only JSONL 落盘，是 gsh 的第一公民。</p>
      <h2>恢复 / 分叉</h2>
      <ul>
        <li><code>-s &lt;id&gt;</code>：进入终端 UI 并定位到该会话，回放历史后继续提问。</li>
        <li><code>--fork &lt;id&gt;</code>：以某会话为蓝本复制一份新会话后进入（原会话不受影响）。</li>
        <li>终端内 <span className="kbd">Ctrl+P</span> 打开会话列表，↑/↓ 选择，Enter 打开，d 删除。</li>
      </ul>
      <h2>回放 / 分享</h2>
      <ul>
        <li><code>-replay &lt;id&gt;</code>：在终端打印该会话的 Trajectory 文本（用户块 / 助手 / 工具行 + diff）后退出，不依赖 LLM。</li>
        <li><code>-replay &lt;id&gt; -w</code>：打印 Web 只读分享链接 <code>https://&lt;host&gt;/#/share/&lt;id&gt;</code>，对方在浏览器打开即可只读查看（对话 + 轨迹）。</li>
      </ul>
      <h2>只读浏览</h2>
      <ul>
        <li><code>-view &lt;id&gt;</code> 或会话内 <code>/readonly</code>：进入只读模式，可滚动、可切会话，但禁止发送。</li>
      </ul>
      <h2>导出</h2>
      <ul>
        <li><code>/export</code> 或 <span className="kbd">Ctrl+E</span>：把当前会话导出为 Markdown 文件（<code>gsh-session-&lt;id&gt;.md</code>），落盘到当前工作目录，便于归档或分享。</li>
      </ul>
    </div>
  )
}

function Dev() {
  return (
    <div>
      <h1>开发</h1>
      <h2>构建与测试</h2>
      <pre><code>{`go build ./...
go vet ./...
go test ./...

# 本地跑 TUI / Web 前先编译
go build -o gsh . && ./gsh -tui`}</code></pre>
      <h2>目录结构</h2>
      <pre><code>{`gsh/
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
  docs-site/           本文档站（Vite + React），部署到 GitHub Pages`}</code></pre>
      <h2>文档站部署</h2>
      <p>
        本站点（<code>docs-site/</code>）是 Vite + React 项目，构建产物通过 GitHub Pages 发布到
        <code>gsh.zgric.top</code>（自定义域名）。推送 <code>main</code> 后由 CI 自动构建并部署到 <code>gh-pages</code> 分支。
      </p>
      <div className="footer">gsh · MIT License · 由 zc team 维护</div>
    </div>
  )
}

const views = {
  intro: Intro,
  quickstart: QuickStart,
  cli: CLI,
  tui: TUI,
  architecture: Architecture,
  config: Config,
  sessions: Sessions,
  dev: Dev,
}

export default function App() {
  const [active, setActive] = useState(
    () => location.hash.replace('#/', '') || 'intro'
  )
  useEffect(() => {
    const onHash = () => setActive(location.hash.replace('#/', '') || 'intro')
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])
  const View = views[active] || Intro
  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="logo">gsh<span className="dot"> ·</span></div>
        <div className="tag">微内核 · 插件化 · 终端式 AI Agent</div>
        <nav className="nav">
          {sections.map((s) => (
            <a
              key={s.id}
              href={'#/' + s.id}
              className={active === s.id ? 'active' : ''}
            >
              {s.label}
            </a>
          ))}
        </nav>
      </aside>
      <main className="content">
        <View />
      </main>
    </div>
  )
}
