/* gsh Web UI
 *
 * 结构对齐 DeepSeek Harness 的对话界面：
 * 左侧会话列表 + 中栏标题/页签/消息流 + 底部输入区与统计。
 *
 * 无框架：所有节点用 DOM API 构造，避免 XSS 且省掉构建链。
 * Markdown 由 md.js 负责（window.renderMarkdown）。
 */
(function () {
  'use strict';

  const $ = (s, r = document) => r.querySelector(s);
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  };
  const svg = (id, size = 14) => {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    s.setAttribute('width', size);
    s.setAttribute('height', size);
    const u = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    u.setAttribute('href', '#' + id);
    s.appendChild(u);
    return s;
  };

  // ---------- state ----------

  const S = {
    model: '',
    provider: '',
    baseUrl: '',
    workspace: '',
    workspace_restrict: true,
    sessions: [],
    messages: [],
    currentId: null,
    running: false,
    runId: null,
    turns: 0,
    steps: 0,
    startedAt: 0,
    abort: null,
    lastTurn: null,      // {root, msgEl, text}
    pendingTools: [],    // 流式过程中的工具行
    permission: 'standard',
    attachments: [],     // 待发送附件 {name,size,text}
    search: '',          // 侧栏会话搜索关键字
    filter: 'all',       // 会话时间筛选：all|today|3d
  };

  const frame = $('#frame');
  const chatEl = $('#chat');
  const chatScroll = $('#chatScroll');
  const input = $('#input');
  const sendBtn = $('#sendBtn');

  // ---------- helpers ----------

  const pad = (n) => String(n).padStart(2, '0');

  // 工作区在侧栏只显示末级目录名（与 DSH 一致），完整路径放 tooltip。
  function baseName(p) {
    if (!p) return '';
    const parts = String(p).split(/[\\\/]/).filter(Boolean);
    return parts.length ? parts[parts.length - 1] : p;
  }

  function timeLabel(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return '';
    const now = new Date();
    const sameDay =
      d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    return sameDay ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : `${d.getMonth() + 1}月${d.getDate()}日`;
  }

  function ageLabel(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return '';
    const min = Math.floor((Date.now() - d.getTime()) / 60000);
    if (min < 1) return '刚刚';
    if (min < 60) return min + '分钟';
    const h = Math.floor(min / 60);
    if (h < 24) return h + '小时';
    const day = Math.floor(h / 24);
    if (day < 30) return day + '天';
    return Math.floor(day / 30) + '个月';
  }

  function toast(msg, kind) {
    const layer = $('#toastLayer');
    const t = el('div', 'toast', msg);
    if (kind) t.dataset.kind = kind;
    layer.appendChild(t);
    setTimeout(() => t.remove(), 3200);
  }

  function scrollToBottom(force) {
    const nearBottom = chatScroll.scrollHeight - chatScroll.scrollTop - chatScroll.clientHeight < 120;
    if (force || nearBottom) chatScroll.scrollTop = chatScroll.scrollHeight;
  }

  async function api(path, opts) {
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = { error: text };
    }
    if (!res.ok) throw new Error((data && data.error) || res.status + ' ' + res.statusText);
    return data;
  }

  // ---------- sidebar ----------

  function renderSidebar() {
    const list = $('#wsList');
    list.innerHTML = '';

    const group = el('div', 'wsGroup');
    const ws = el('button', 'wsItem');
    ws.appendChild(svg('i-file', 13));
    const name = el('span', 'wsName', baseName(S.workspace) || '默认工作区');
    ws.appendChild(name);
    ws.title = S.workspace || '';
    ws.onclick = () => openWorkspacePicker(S.workspace);
    group.appendChild(ws);

    const q = (S.search || '').trim().toLowerCase();
    const f = FILTERS.find((x) => x[0] === S.filter) || FILTERS[0];
    const byTime = S.sessions.filter(f[2]);
    const shown = q
      ? byTime.filter((x) => String(x.title || '').toLowerCase().includes(q)).slice(0, 30)
      : byTime.slice(0, 12);
    shown.forEach((s) => {
      const item = el('button', 'convItem');
      item.dataset.id = s.id;
      if (s.id === S.currentId) item.dataset.active = 'true';
      item.appendChild(el('span', 'convName', s.title || '新会话'));
      if (s.updated) item.appendChild(el('span', 'convAge', ageLabel(s.updated)));
      item.onclick = () => openSession(s.id);
      item.oncontextmenu = (e) => {
        e.preventDefault();
        openConvMenu(e, s);
      };
      group.appendChild(item);
    });

    list.appendChild(group);
    $('#wsPickerLabel').textContent = baseName(S.workspace) || '选择一个工作区';
  }

  // ---------- chat rendering ----------

  function clearChat() {
    chatEl.innerHTML = '';
    S.lastTurn = null;
    S.pendingTools = [];
  }

  function newTurn() {
    const root = el('div', 'turn');
    chatEl.appendChild(root);
    S.lastTurn = { root, msgEl: null, text: '', tools: [] };
    return S.lastTurn;
  }

  function appendUser(content, iso, atts) {
    const t = newTurn();
    const row = el('div', 'userRow');

    // 附件内容在发送时拼进了 prompt；回显时把它们抽回卡片，
    // 免得气泡里一大段原文把真正的提问挤没。
    content = String(content == null ? '' : content);
    const list = (atts || []).slice();
    content = content
      .replace(/\n--- 附件 ([^\n]*?)---\n([\s\S]*?)\n--- 附件结束 ---/g, (m, head, body) => {
        const name = head.replace(/（[^）]*）$/, '').trim();
        list.push({ name: name, size: body.length, text: body });
        return '';
      })
      .trim();

    if (list.length) {
      const cards = el('div', 'attachCards');
      list.forEach((a) => {
        const c = el('div', 'attachCard');
        c.appendChild(svg('i-file', 12));
        c.appendChild(el('span', null, a.name));
        c.appendChild(el('span', 'attachSize', fmtSize(a.size)));
        cards.appendChild(c);
      });
      row.appendChild(cards);
    }
    row.appendChild(el('div', 'userBubble', content));
    if (iso) row.appendChild(el('div', 'userMeta', timeLabel(iso)));
    const ucopy = el('button', 'msgCopy');
    ucopy.dataset.target = 'user';
    ucopy.appendChild(svg('i-copy', 11));
    ucopy.title = '复制';
    row.appendChild(ucopy);
    t.root.appendChild(row);
    return t;
  }

  function ensureAssistant(t) {
    if (!t.msgEl) {
      t.msgEl = el('div', 'msgWrap');
      const acopy = el('button', 'msgCopy');
      acopy.dataset.target = 'assistant';
      acopy.appendChild(svg('i-copy', 11));
      acopy.title = '复制';
      t.msgEl.appendChild(acopy);
      const body = el('div', 'msg');
      t.msgEl.appendChild(body);
      t.msgBody = body;
      t.root.appendChild(t.msgEl);
    }
    return t.msgBody || t.msgEl;
  }

  function appendAssistantText(t, text) {
    const m = ensureAssistant(t);
    t.text += text;
    m.innerHTML = window.renderMarkdown(t.text);
    scrollToBottom();
  }

  function setAssistantHTML(t, html) {
    const m = ensureAssistant(t);
    m.innerHTML = html;
    scrollToBottom();
  }

  // 工具调用行：默认折叠，点开看参数与结果。
  function appendToolRow(t, name, args) {
    const row = el('div', 'toolRow');
    row.dataset.state = 'running';
    row.dataset.open = 'false';
    row.appendChild(svg('i-terminal', 13)).classList.add('toolIcon');
    row.appendChild(el('span', 'toolName', name));
    row.appendChild(el('span', 'toolSummary', summarizeArgs(args)));
    row.appendChild(el('span', 'toolState', '执行中'));

    const detail = el('div', 'toolDetail');
    detail.appendChild(el('div', 'toolLabel', '参数'));
    const preArgs = el('pre', null, prettyArgs(args));
    detail.appendChild(preArgs);
    const label = el('div', 'toolLabel', '结果');
    label.style.display = 'none';
    detail.appendChild(label);
    const preRes = el('pre');
    preRes.style.display = 'none';
    detail.appendChild(preRes);

    row.onclick = () => {
      row.dataset.open = row.dataset.open === 'true' ? 'false' : 'true';
    };

    t.root.appendChild(row);
    t.root.appendChild(detail);

    // 一段文本后接了工具调用，说明这一小段到此为止；
    // 下一段 delta 必须开新块，否则后续回复会拼到同一段里。
    t.msgEl = null;

    const entry = { row, detail, preRes, label, name };
    t.tools.push(entry);
    return entry;
  }

  function finishToolRow(entry, result, isError) {
    entry.row.dataset.state = isError ? 'error' : 'done';
    entry.row.querySelector('.toolState').textContent = isError ? '失败' : '完成';
    entry.label.style.display = '';
    entry.preRes.style.display = '';
    entry.preRes.textContent = clip(result, 8000);
    if (isError) entry.row.dataset.open = 'true';
  }

  function summarizeArgs(args) {
    if (!args) return '';
    try {
      const o = JSON.parse(args);
      const v =
        o.command || o.path || o.pattern || o.needle || o.url || o.name ||
        (Array.isArray(o.argv) && o.argv.join(' ')) || '';
      if (v) return String(v).split('\n')[0].slice(0, 120);
      const keys = Object.keys(o);
      if (!keys.length) return '';
      return keys.slice(0, 3).join(', ');
    } catch {
      /* 参数不是合法 JSON 时直接展示原文 */
    }
    return String(args).split('\n')[0].slice(0, 120);
  }

  function prettyArgs(args) {
    if (!args) return '';
    try {
      return JSON.stringify(JSON.parse(args), null, 2);
    } catch {
      return String(args);
    }
  }

  function clip(s, n) {
    s = String(s == null ? '' : s);
    return s.length > n ? s.slice(0, n) + '\n…[truncated]' : s;
  }

  // 从历史 messages 重建整段对话。
  function renderHistory(messages) {
    clearChat();
    let t = null;
    const pending = []; // 尚未拿到结果的工具行

    messages.forEach((m) => {
      if (m.role === 'user') {
        t = appendUser(m.content || '');
      } else if (m.role === 'assistant') {
        if (!t) t = newTurn();
        if (m.content) setAssistantHTML(t, window.renderMarkdown(m.content));
        (m.tool_calls || []).forEach((c) => {
          const entry = appendToolRow(t, c.name, c.arguments);
          pending.push({ id: c.id, entry });
        });
      } else if (m.role === 'tool') {
        // 按 tool_call_id 精确配对；缺失时退化为「最近一个未完成」。
        let idx = pending.findIndex((p) => p.id && p.id === m.tool_call_id);
        if (idx < 0) idx = pending.findIndex((p) => p.entry.name === m.name);
        if (idx >= 0) {
          const p = pending.splice(idx, 1)[0];
          const isErr = /^(error|exit:|.*错误)/i.test(String(m.content || ''));
          finishToolRow(p.entry, m.content || '', isErr);
        }
      }
    });
    scrollToBottom(true);
  }

  // ---------- session flow ----------

  async function openSession(id) {
    if (S.running) {
      toast('正在运行，先停止当前任务');
      return;
    }
    try {
      const data = await api('/api/session?id=' + encodeURIComponent(id));
      S.currentId = id;
      frame.dataset.view = 'chat';
      const meta = S.sessions.find((s) => s.id === id);
      $('#convTitle').textContent = (meta && meta.title) || '新会话';
      S.messages = data.messages || [];
      renderHistory(S.messages);
      renderSidebar();
      S.turns = countTurns(data.messages || []);
      S.steps = countToolCalls(data.messages || []);
      updateStats();
      input.focus();
    } catch (e) {
      toast('打开会话失败：' + e.message, 'error');
    }
  }

  // 只读分享：从 URL #/share/<id> 进入，加载会话并自动切到轨迹页，隐藏输入区。
  async function loadShare(id) {
    try {
      const data = await api('/api/session?id=' + encodeURIComponent(id));
      S.currentId = id;
      const meta = S.sessions.find((s) => s.id === id);
      $('#convTitle').textContent = (meta && meta.title) || '新会话 · 只读分享';
      S.messages = data.messages || [];
      renderHistory(S.messages);
      renderSidebar();
      S.turns = countTurns(S.messages);
      S.steps = countToolCalls(S.messages);
      updateStats();
      frame.dataset.view = 'chat';
      const tTab = $('#tabTrajectory');
      if (tTab) tTab.click();
      frame.classList.add('shareMode');
      input.blur();
      document.title = 'gsh 轨迹分享 · ' + ((meta && meta.title) || id);
      toast('只读分享模式：可滚动查看，不能发送');
    } catch (e) {
      toast('打开会话失败：' + e.message, 'error');
    }
  }

  function countTurns(msgs) {
    return msgs.filter((m) => m.role === 'user').length;
  }
  function countToolCalls(msgs) {
    return msgs.reduce((n, m) => n + ((m.tool_calls && m.tool_calls.length) || 0), 0);
  }

  function newSession() {
    if (S.running) {
      toast('正在运行，先停止当前任务');
      return;
    }
    S.currentId = null;
    frame.dataset.view = 'empty';
    clearChat();
    $('#convTitle').textContent = '新会话';
    S.turns = 0;
    S.steps = 0;
    updateStats();
    renderSidebar();
    input.focus();
  }

  async function deleteSession(id) {
    try {
      await api('/api/session/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      });
      if (S.currentId === id) newSession();
      await refresh();
    } catch (e) {
      toast('删除失败：' + e.message, 'error');
    }
  }

  async function refresh() {
    try {
      const st = await api('/api/state');
      S.model = st.model || '';
      S.provider = st.provider || '';
      S.baseUrl = st.base_url || '';
      S.thinkingFormat = st.thinking_format || 'none';
      S.apiKeySet = !!st.api_key_set;
      S.workspace = st.workspace || '';
      S.workspace_restrict = st.workspace_restrict !== false;
      S.permission = st.permission || 'standard';
      S.sessions = st.sessions || [];
      $('#modelName').textContent = S.model || '未配置模型';
      updatePermBtn();
      renderSidebar();
    } catch (e) {
      toast('加载失败：' + e.message, 'error');
    }
  }

  // ---------- send ----------

  async function send() {
    const text = input.value.trim();
    if ((!text && !S.attachments.length) || S.running) return;

    // 未选会话时先建一个，保证消息有落点。
    if (!S.currentId) {
      try {
        const r = await api('/api/session/new', { method: 'POST' });
        S.currentId = r.id;
      } catch (e) {
        toast('创建会话失败：' + e.message, 'error');
        return;
      }
    }

    frame.dataset.view = 'chat';
    let prompt, atts, t;
    try {
      prompt = composePrompt(text);
      atts = attachmentSummary();
    } catch (e) {
      toast('消息组装失败：' + e.message, 'error');
      return;
    }
    input.value = '';
    S.attachments = [];
    renderAttach();
    autoGrow();
    setRunning(true);

    t = appendUser(text || '（仅附件）', new Date().toISOString(), atts);
    S.turns += 1;
    if (S.turns === 1) $('#convTitle').textContent = text.slice(0, 42);
    updateStats();
    scrollToBottom(true);

    const runId = 'run-' + Date.now().toString(36);
    S.runId = runId;
    const ctrl = new AbortController();
    S.abort = ctrl;

    try {
      const res = await fetch('/api/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ prompt: prompt, session_id: S.currentId, run_id: runId }),
        signal: ctrl.signal,
      });
      if (!res.ok || !res.body) throw new Error('HTTP ' + res.status);

      await consumeSSE(res.body, (ev, data) => {
        if (ev === 'delta') {
          appendAssistantText(t, data.text || '');
        } else if (ev === 'tool_call') {
          const entry = appendToolRow(t, data.name, data.args);
          entry.id = data.id || '';
          S.pendingTools.push(entry);
          S.steps += 1;
          updateStats();
        } else if (ev === 'tool_result') {
          // 优先按调用 ID 配对：同一轮可能连续调用同名工具（如两次 bash）。
          let idx = data.id ? S.pendingTools.findIndex((p) => p.id === data.id) : -1;
          if (idx < 0) idx = S.pendingTools.findIndex((p) => p.name === data.name);
          if (idx >= 0) {
            const entry = S.pendingTools.splice(idx, 1)[0];
            const isErr = data.err === true || /^(error|exit:)/i.test(String(data.data || ''));
            finishToolRow(entry, data.data || '', isErr);
          }
          scrollToBottom();
        } else if (ev === 'final') {
          if (data.data && !t.text) setAssistantHTML(t, window.renderMarkdown(data.data));
          scrollToBottom();
        } else if (ev === 'error') {
          t.hadError = true;
          appendStatus(t, 'error', '运行失败', data.message);
        } else if (ev === 'stopped') {
          t.hadError = true;
          appendStatus(t, 'stopped', '已停止', '用户主动中断');
        } else if (ev === 'done') {
          if (!t.hadError && S.startedAt) {
            appendStatus(t, 'done', '已完成', '用时 ' + fmtDur(Math.floor((Date.now() - S.startedAt) / 1000)));
          }
        }
      });
    } catch (e) {
      if (e.name === 'AbortError') {
        appendStatus(t, 'stopped', '已停止', '用户主动中断');
      } else {
        appendStatus(t, 'error', '请求失败', e.message);
      }
    } finally {
      // 收尾：仍有未回填的工具行标为未知
      S.pendingTools.forEach((p) => {
        p.row.dataset.state = 'error';
        p.row.querySelector('.toolState').textContent = '未完成';
      });
      S.pendingTools = [];
      setRunning(false);
      S.abort = null;
      S.runId = null;
      // 静默同步一次权威消息列表，保证轨迹视图与刷新后一致
      try {
        const d = await api('/api/session?id=' + encodeURIComponent(S.currentId));
        S.messages = d.messages || [];
        if (document.querySelector('.tab.active').dataset.tab === 'trajectory') renderTrace();
      } catch {
        /* 会话可能刚被删除 */
      }
      refresh();
    }
  }

  function appendStatus(t, kind, label, detail) {
    const row = el('div', 'statusRow');
    row.dataset.kind = kind === 'stopped' ? 'done' : kind;
    row.appendChild(el('span', 'dot'));
    row.appendChild(el('span', null, label));
    if (detail) row.appendChild(el('span', 'detail', '— ' + detail));
    t.root.appendChild(row);
    scrollToBottom();
  }

  // 手写 SSE 解析：POST 流不能用 EventSource。
  async function consumeSSE(body, onEvent) {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const chunk = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let event = 'message';
        let dataStr = '';
        chunk.split('\n').forEach((line) => {
          if (line.startsWith('event:')) event = line.slice(6).trim();
          else if (line.startsWith('data:')) dataStr += line.slice(5).trim();
        });
        if (!dataStr) continue;
        let data;
        try {
          data = JSON.parse(dataStr);
        } catch {
          data = { raw: dataStr };
        }
        onEvent(event, data);
      }
    }
  }

  function setRunning(v) {
    S.running = v;
    sendBtn.dataset.mode = v ? 'stop' : 'send';
    sendBtn.innerHTML = '';
    sendBtn.appendChild(svg(v ? 'i-stop' : 'i-send', 16));
    sendBtn.title = v ? '停止' : '发送';
    input.disabled = false;
    if (v) {
      S.startedAt = Date.now();
      updateStats();
    }
  }

  function updateStats() {
    const parts = [];
    parts.push(S.turns + ' 轮');
    if (S.steps) parts.push(S.steps + ' 步');
    if (S.running && S.startedAt) {
      const sec = Math.floor((Date.now() - S.startedAt) / 1000);
      parts.push(fmtDur(sec));
    }
    if (S.model) parts.push(S.model);
    const bar = $('#statsBar');
    bar.innerHTML = '';
    parts.forEach((p, i) => {
      if (i) bar.appendChild(el('span', 'sep', '·'));
      bar.appendChild(el('span', null, p));
    });
  }

  function fmtDur(sec) {
    if (sec < 60) return sec + 's';
    return Math.floor(sec / 60) + 'm ' + (sec % 60) + 's';
  }

  // ---------- composer ----------

  function autoGrow() {
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight, 220) + 'px';
    sendBtn.dataset.ready = input.value.trim() ? 'true' : 'false';
  }

  input.addEventListener('input', autoGrow);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  });

  sendBtn.onclick = () => {
    if (S.running) stopRun();
    else send();
  };

  async function stopRun() {
    if (!S.runId) return;
    try {
      await api('/api/stop', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ run_id: S.runId }),
      });
    } catch (e) {
      // 后端已经结束时 404 是正常情况
    }
  }

  // ---------- attachments ----------
  //
  // 附件走纯前端读文本，随消息一起送进 prompt：
  // harness 不是文件中转站，模型真正需要的是「内容能进上下文」。
  // 大文件/二进制在这里就拒绝，不给后端留一个必炸的入口。
  const ATTACH_MAX = 256 * 1024; // 单文件上限

  const attachRow = $('#attachRow');
  const attachInput = $('#attachInput');

  function fmtSize(n) {
    if (n < 1024) return n + 'B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + 'KB';
    return (n / 1024 / 1024).toFixed(1) + 'MB';
  }

  function renderAttach() {
    attachRow.innerHTML = '';
    if (!S.attachments.length) {
      attachRow.hidden = true;
      return;
    }
    attachRow.hidden = false;
    S.attachments.forEach((a, i) => {
      const chip = el('div', 'attachChip');
      chip.appendChild(svg('i-file', 12));
      chip.appendChild(el('span', 'attachName', a.name));
      chip.appendChild(el('span', 'attachSize', fmtSize(a.size)));
      const x = el('button', 'attachRemove');
      x.appendChild(svg('i-x', 10));
      x.onclick = () => {
        S.attachments.splice(i, 1);
        renderAttach();
        autoGrow();
      };
      chip.appendChild(x);
      attachRow.appendChild(chip);
    });
    autoGrow();
  }

  attachInput.onchange = async () => {
    for (const f of attachInput.files) {
      if (S.attachments.length >= 8) {
        toast('一次最多 8 个附件', 'error');
        break;
      }
      if (S.attachments.some((a) => a.name === f.name && a.size === f.size)) {
        continue; // 同名同大小视为重复
      }
      if (f.size > ATTACH_MAX) {
        toast(f.name + ' 超过 ' + fmtSize(ATTACH_MAX) + '，未添加', 'error');
        continue;
      }
      try {
        const text = await f.text();
        if (text.indexOf('\u0000') >= 0) {
          toast(f.name + ' 是二进制文件，未添加', 'error');
          continue;
        }
        S.attachments.push({ name: f.name, size: f.size, text: text });
      } catch (e) {
        toast('读取 ' + f.name + ' 失败：' + e.message, 'error');
      }
    }
    attachInput.value = '';
    renderAttach();
  };

  // 附件内容拼进 prompt：让模型直接看到，而不是猜文件里有什么。
  function composePrompt(text) {
    if (!S.attachments.length) return text;
    const parts = [text];
    S.attachments.forEach((a) => {
      const head = '\n--- 附件 ' + a.name + '（' + fmtSize(a.size) + '）---\n';
      const tail = '\n--- 附件结束 ---';
      parts.push(head + a.text + tail);
    });
    return parts.join('');
  }

  // 发送时随消息带上附件元信息，回显卡片用（内容已在 composePrompt 里）。
  function attachmentSummary() {
    return S.attachments.map((a) => ({ name: a.name, size: a.size }));
  }

  $('#newSessionBtn').onclick = newSession;

  $('#collapseBtn').onclick = () => {
    frame.dataset.animating = '1';
    frame.dataset.sidebar = frame.dataset.sidebar === 'collapsed' ? 'expanded' : 'collapsed';
    setTimeout(() => delete frame.dataset.animating, 320);
  };

  $('#modelBtn').onclick = openSettings;
  $('#permBtn').onclick = openPermission;
  $('#attachBtn').onclick = () => $('#attachInput').click();
  $('#addWorkspace').onclick = () => openWorkspacePicker(S.workspace);
  $('#wsPicker').onclick = () => openWorkspacePicker(S.workspace);

  // ---------- workspace picker ----------

  async function openWorkspacePicker(start) {
    if ($('.modalMask')) return;

    const mask = el('div', 'modalMask');
    const modal = el('div', 'modal');
    modal.appendChild(el('div', 'modalTitle', '选择工作区'));
    const desc = el('div', 'modalDesc', 'Agent 的所有相对路径与命令都以工作区为基准。');
    modal.appendChild(desc);

    const pathBar = el('div', 'wsPathBar');
    const upBtn = el('button', 'btn', '↑ 上级');
    const pathText = el('span', 'wsPath', '');
    pathBar.appendChild(upBtn);
    pathBar.appendChild(pathText);
    modal.appendChild(pathBar);

    const list = el('div', 'wsBrowse');
    modal.appendChild(list);

    const restrict = el('label', 'wsRestrict');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = S.workspace_restrict !== false;
    restrict.appendChild(cb);
    restrict.appendChild(el('span', null, '限制工具只能访问工作区内（拦绝对路径越界与符号链接）'));
    modal.appendChild(restrict);
    cb.onchange = async () => {
      try {
        await api('/api/workspace/restrict', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ restrict: cb.checked }),
        });
        S.workspace_restrict = cb.checked;
        toast(cb.checked ? '路径白名单已开启' : '路径白名单已关闭');
      } catch (e) {
        cb.checked = !cb.checked;
        toast('切换失败：' + e.message, 'error');
      }
    };

    const foot = el('div', 'modalFoot');
    const cancel = el('button', 'btn', '取消');
    const pick = el('button', 'btn primary', '使用此目录');
    foot.appendChild(cancel);
    foot.appendChild(pick);
    modal.appendChild(foot);

    mask.appendChild(modal);
    document.body.appendChild(mask);
    const close = () => mask.remove();
    cancel.onclick = close;
    mask.onclick = (e) => {
      if (e.target === mask) close();
    };

    let cur = '';

    async function load(p) {
      list.innerHTML = '';
      list.appendChild(el('div', 'wsLoading', '加载中…'));
      try {
        const d = await api('/api/workspace/browse?path=' + encodeURIComponent(p || ''));
        cur = d.path;
        pathText.textContent = d.path;
        pathText.title = d.path;
        upBtn.onclick = () => load(d.parent);
        list.innerHTML = '';
        if (!d.dirs.length) {
          list.appendChild(el('div', 'wsLoading', '（没有子目录）'));
        }
        d.dirs.forEach((dir) => {
          const row = el('button', 'wsRow');
          row.appendChild(svg('i-file', 13));
          row.appendChild(el('span', null, dir.name));
          row.onclick = () => load(dir.path);
          list.appendChild(row);
        });
      } catch (e) {
        list.innerHTML = '';
        list.appendChild(el('div', 'wsLoading', '读取失败：' + e.message));
      }
    }

    pick.onclick = async () => {
      try {
        await api('/api/workspace', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: cur }),
        });
        S.workspace = cur;
        renderSidebar();
        close();
        toast('工作区已切换：' + cur);
      } catch (e) {
        toast('切换失败：' + e.message, 'error');
      }
    };

    load(start || '');
  }
  $('#btnJson').onclick = async () => {
    if (!S.currentId) return;
    try {
      const d = await api('/api/session?id=' + encodeURIComponent(S.currentId));
      const w = window.open('', '_blank');
      w.document.write('<pre style="white-space:pre-wrap;font:12px monospace;padding:16px">' +
        window.escapeHtml(JSON.stringify(d, null, 2)) + '</pre>');
    } catch (e) {
      toast('读取失败：' + e.message, 'error');
    }
  };
  // ---------- 会话右键菜单与搜索 ----------
  //
  // 右键不是「只能删除」：重命名是会话管理的基本操作，
  // 会话多了以后，自动摘要出来的标题根本认不出哪个是哪个。

  function closeConvMenu() {
    const m = $('.contextMenu');
    if (m) m.remove();
    document.removeEventListener('click', closeConvMenu);
  }

  function openConvMenu(e, meta) {
    closeConvMenu();
    const menu = el('div', 'contextMenu');
    menu.style.left = Math.min(e.clientX, window.innerWidth - 180) + 'px';
    menu.style.top = Math.min(e.clientY, window.innerHeight - 110) + 'px';

    const rename = el('button', 'ctxItem', '重命名');
    rename.onclick = () => {
      closeConvMenu();
      const next = prompt('会话标题', meta.title || '');
      if (next === null) return;
      api('/api/session/rename', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: meta.id, title: next }),
      })
        .then(() => {
          if (S.currentId === meta.id) $('#convTitle').textContent = next.trim() || '新会话';
          return refresh();
        })
        .catch((err) => toast('重命名失败：' + err.message, 'error'));
    };

    const del = el('button', 'ctxItem danger', '删除');
    del.onclick = () => {
      closeConvMenu();
      if (confirm(`删除会话「${meta.title || '新会话'}」？此操作不可恢复。`)) deleteSession(meta.id);
    };

    menu.appendChild(rename);
    menu.appendChild(del);
    document.body.appendChild(menu);
    setTimeout(() => document.addEventListener('click', closeConvMenu), 0);
  }

  // 侧栏搜索：输入即过滤会话标题。
  function toggleSearch() {
    let box = $('#wsSearch');
    if (box) {
      S.search = '';
      box.remove();
      renderSidebar();
      return;
    }
    box = el('input', 'wsSearch');
    box.id = 'wsSearch';
    box.placeholder = '搜索会话…';
    const header = document.querySelector('.wsHeader');
    header.parentNode.insertBefore(box, header.nextSibling);
    box.focus();
    box.oninput = () => {
      S.search = box.value;
      renderSidebar();
    };
    box.onkeydown = (e) => {
      if (e.key === 'Escape') {
        S.search = '';
        box.remove();
        renderSidebar();
      }
    };
  }

  // 筛选：按时间圈定侧栏会话范围。会话一多，「全部」列表就没法用。
  const FILTERS = [
    ['all', '全部会话', () => true],
    ['today', '今天', (x) => Date.now() - new Date(x.updated).getTime() < 86400e3],
    ['3d', '最近三天', (x) => Date.now() - new Date(x.updated).getTime() < 3 * 86400e3],
  ];

  function openFilterMenu(e) {
    closeConvMenu();
    const menu = el('div', 'contextMenu');
    menu.style.left = Math.min(e.clientX, window.innerWidth - 180) + 'px';
    menu.style.top = Math.min(e.clientY, window.innerHeight - 140) + 'px';
    FILTERS.forEach(([key, label]) => {
      const it = el('button', 'ctxItem', (S.filter === key ? '· ' : '') + label);
      it.onclick = () => {
        S.filter = key;
        closeConvMenu();
        renderSidebar();
        toast('筛选：' + label);
      };
      menu.appendChild(it);
    });
    document.body.appendChild(menu);
    setTimeout(() => document.addEventListener('click', closeConvMenu), 0);
  }

  document.querySelectorAll('.wsHeaderActions .iconBtn').forEach((b, i) => {
    if (i === 0) b.onclick = toggleSearch;
    else if (i === 1) b.onclick = (e) => openFilterMenu(e);
  });

  // ---------- drawer：能力与环境 ----------
  //
  // 工具规格、技能、运行诊断都在这里。它回答的是「它到底能干什么、
  // 现在跑在什么环境里」——这类信息不该只能靠 -tools 在终端看。

  const drawer = $('#drawer');

  function setDrawer(open) {
    drawer.dataset.open = open ? 'true' : 'false';
    // 面板是挤压布局，不是盖在输入区上的浮层
    frame.dataset.drawer = open ? 'true' : 'false';
  }

  async function openDrawer() {
    setDrawer(true);
    const body = $('#drawerBody');
    if (body.dataset.loaded === '1') return;
    body.innerHTML = '';
    body.appendChild(el('div', 'drawerLoading', '加载中…'));
    try {
      const d = await api('/api/tools');
      body.dataset.loaded = '1';
      body.innerHTML = '';

      const toolsSec = el('div', 'drawerSection');
      toolsSec.appendChild(el('div', 'drawerSectionHead', '工具 · ' + (d.tools || []).length));
      (d.tools || []).forEach((t) => {
        const card = el('div', 'toolCard');
        card.appendChild(el('div', 'toolCardName', t.name));
        card.appendChild(el('div', 'toolCardDesc', t.description || ''));
        toolsSec.appendChild(card);
      });
      body.appendChild(toolsSec);

      const skillsSec = el('div', 'drawerSection');
      skillsSec.appendChild(el('div', 'drawerSectionHead', '技能 · ' + (d.skills || []).length));
      if (!(d.skills || []).length) {
        skillsSec.appendChild(el('div', 'toolCardDesc', '（skills 目录里还没有 SKILL.md）'));
      }
      (d.skills || []).forEach((sk) => {
        const card = el('div', 'skillCard');
        card.appendChild(el('div', 'skillCardName', sk.name));
        if (sk.description) card.appendChild(el('div', 'skillCardDesc', sk.description));
        skillsSec.appendChild(card);
      });
      body.appendChild(skillsSec);

      const diagSec = el('div', 'drawerSection');
      diagSec.appendChild(el('div', 'drawerSectionHead', '运行诊断'));
      diagSec.appendChild(el('div', 'diagBox', d.diag || '（无诊断信息）'));
      body.appendChild(diagSec);
    } catch (e) {
      body.innerHTML = '';
      body.appendChild(el('div', 'drawerLoading', '加载失败：' + e.message));
    }
  }

  $('#btnRight').onclick = () => {
    if (drawer.dataset.open === 'true') setDrawer(false);
    else openDrawer();
  };
  $('#drawerClose').onclick = () => setDrawer(false);
  $('#navPlugins').onclick = openDrawer;

  // ---------- permission ----------

  // 权限档位：三档，对应工具层的 Guard。
  // 切换后下一次工具调用立即生效，不需要重启。
  const PERM_OPTIONS = [
    ['read_only', '只读', '仅读取与网络访问。不执行命令、不改文件。'],
    ['standard', '标准', '可读写文件与执行命令。破坏性命令仍被拦截。'],
    ['full', '完全权限', '破坏性命令放行。灾难性命令（如 rm -rf /）仍然拦截。'],
  ];

  function permLabelOf(mode) {
    const hit = PERM_OPTIONS.find((o) => o[0] === mode);
    return hit ? hit[1] : '标准';
  }

  function updatePermBtn() {
    const btn = $('#permBtn');
    btn.querySelector('span').textContent = permLabelOf(S.permission);
    btn.title = '权限：' + permLabelOf(S.permission);
  }

  function openPermission() {
    if ($('.modalMask')) return;

    const mask = el('div', 'modalMask');
    const modal = el('div', 'modal');
    modal.appendChild(el('div', 'modalTitle', '权限'));
    modal.appendChild(el('div', 'modalDesc', '决定 Agent 能对这台机器做什么。切换后下一次工具调用即生效。'));

    const list = el('div', 'permList');
    PERM_OPTIONS.forEach(([mode, label, desc]) => {
      const row = el('button', 'permRow');
      if (mode === S.permission) row.dataset.active = 'true';
      const head = el('div', 'permHead');
      head.appendChild(el('span', 'permName', label));
      if (mode === S.permission) head.appendChild(el('span', 'permNow', '当前'));
      row.appendChild(head);
      row.appendChild(el('div', 'permDesc', desc));
      row.onclick = async () => {
        if (mode === S.permission) return;
        try {
          const d = await api('/api/permission', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ mode }),
          });
          S.permission = d.mode;
          updatePermBtn();
          close();
          toast('权限已切换：' + (d.label || permLabelOf(d.mode)));
        } catch (e) {
          toast('切换失败：' + e.message, 'error');
        }
      };
      list.appendChild(row);
    });
    modal.appendChild(list);

    const foot = el('div', 'modalFoot');
    const cancel = el('button', 'btn', '关闭');
    foot.appendChild(cancel);
    modal.appendChild(foot);

    mask.appendChild(modal);
    document.body.appendChild(mask);
    const close = () => mask.remove();
    cancel.onclick = close;
    mask.onclick = (e) => {
      if (e.target === mask) close();
    };
    document.addEventListener(
      'keydown',
      function esc(e) {
        if (e.key === 'Escape') {
          close();
          document.removeEventListener('keydown', esc);
        }
      },
      { once: true }
    );
  }

  // ---------- settings ----------

  function openSettings() {
    // 已有遮罩就不重复开
    if ($('.modalMask')) return;

    const mask = el('div', 'modalMask');
    const modal = el('div', 'modal');

    modal.appendChild(el('div', 'modalTitle', '模型设置'));
    modal.appendChild(
      el('div', 'modalDesc', '保存后立即生效并写回 config.json。API Key 不回显，留空表示保持不变。')
    );

    const providerField = el('div', 'field');
    providerField.appendChild(el('label', null, '提供方'));
    const sel = el('select');
    [['openai', 'OpenAI 兼容（openai）'], ['echo', '本地回显（echo，不调用模型）']].forEach(([v, t]) => {
      const o = el('option', null, t);
      o.value = v;
      sel.appendChild(o);
    });
    sel.value = S.provider || 'openai';
    providerField.appendChild(sel);
    modal.appendChild(providerField);

    const baseInput = mkField(modal, '接口地址 base_url', S.baseUrl, 'https://api.deepseek.com');
    const modelInput = mkField(modal, '模型 model', S.model, 'deepseek-chat');
    const keyInput = mkField(modal, 'API Key', '', S.apiKeySet ? '已配置（留空不修改）' : 'sk-…', 'password');
    const thinkInput = mkField(modal, '思考格式 thinking_format', S.thinkingFormat || 'none', 'none / deepseek');

    const foot = el('div', 'modalFoot');
    const cancel = el('button', 'btn', '取消');
    const save = el('button', 'btn primary', '保存');
    foot.appendChild(cancel);
    foot.appendChild(save);
    modal.appendChild(foot);

    mask.appendChild(modal);
    document.body.appendChild(mask);

    const close = () => mask.remove();
    cancel.onclick = close;
    mask.onclick = (e) => {
      if (e.target === mask) close();
    };
    document.addEventListener(
      'keydown',
      function esc(e) {
        if (e.key === 'Escape') {
          close();
          document.removeEventListener('keydown', esc);
        }
      },
      { once: true }
    );

    save.onclick = async () => {
      save.disabled = true;
      save.textContent = '保存中…';
      try {
        const body = {
          provider: sel.value,
          base_url: baseInput.value.trim(),
          model: modelInput.value.trim(),
          thinking_format: thinkInput.value.trim(),
        };
        if (keyInput.value.trim()) body.api_key = keyInput.value.trim();
        await api('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        close();
        toast('已保存，立即生效');
        await refresh();
      } catch (e) {
        save.disabled = false;
        save.textContent = '保存';
        toast('保存失败：' + e.message, 'error');
      }
    };
  }

  function mkField(parent, label, value, placeholder, type) {
    const f = el('div', 'field');
    f.appendChild(el('label', null, label));
    const i = el('input');
    i.value = value || '';
    if (placeholder) i.placeholder = placeholder;
    if (type) i.type = type;
    f.appendChild(i);
    parent.appendChild(f);
    return i;
  }

  // 页签：对话 / 轨迹
  document.querySelectorAll('.tab').forEach((tab) => {
    tab.onclick = () => {
      document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t === tab));
      const trace = tab.dataset.tab === 'trajectory';
      chatScroll.hidden = trace;
      $('#traceScroll').hidden = !trace;
      if (trace) renderTrace();
    };
  });

  // 轨迹视图：把会话里的工具调用按时间线摊开，便于回答「它到底做了什么」。
  function renderTrace() {
    const root = $('#trace');
    root.innerHTML = '';
    const steps = collectSteps(S.messages);

    if (!steps.length) {
      root.appendChild(el('div', 'traceEmpty', '这个会话还没有工具调用'));
      return;
    }

    steps.forEach((s, i) => {
      const row = el('div', 'traceStep');
      row.dataset.state = s.state;
      row.appendChild(el('span', 'traceIdx', String(i + 1)));

      const body = el('div', 'traceBody');
      body.dataset.open = 'false';
      const head = el('div', 'traceHead');
      head.appendChild(el('span', 'traceName', s.name));
      head.appendChild(el('span', 'traceSum', s.summary || ''));
      head.appendChild(el('span', 'traceState', s.state === 'error' ? '失败' : s.state === 'running' ? '进行中' : '完成'));
      head.onclick = () => {
        body.dataset.open = body.dataset.open === 'true' ? 'false' : 'true';
      };
      body.appendChild(head);

      const detail = el('div', 'traceDetail');
      if (s.args) {
        detail.appendChild(el('pre', null, prettyArgs(s.args)));
      }
      if (s.result) {
        detail.appendChild(el('pre', null, clip(s.result, 4000)));
      }
      body.appendChild(detail);

      row.appendChild(body);
      root.appendChild(row);
    });
  }

  function collectSteps(messages) {
    const steps = [];
    const byId = new Map();
    (messages || []).forEach((m) => {
      if (m.role === 'assistant' && m.tool_calls) {
        m.tool_calls.forEach((c) => {
          const s = {
            id: c.id || '',
            name: c.name,
            args: c.arguments,
            summary: summarizeArgs(c.arguments),
            result: '',
            state: 'running',
          };
          steps.push(s);
          if (c.id) byId.set(c.id, s);
        });
      } else if (m.role === 'tool') {
        let s = (m.tool_call_id && byId.get(m.tool_call_id)) || null;
        if (!s) s = steps.filter((x) => x.name === m.name && x.state === 'running').pop();
        if (s) {
          s.result = m.content || '';
          s.state = /^(error|exit:)/i.test(s.result) ? 'error' : 'done';
        }
      }
    });
    return steps;
  }

  // 代码块复制 + 整条消息复制（事件委托）
  chatEl.addEventListener('click', (e) => {
    const msgCopy = e.target.closest('.msgCopy');
    if (msgCopy) {
      const host = msgCopy.dataset.target === 'user' ? msgCopy.closest('.userRow') : msgCopy.closest('.msgWrap');
      const bubble = host && (host.querySelector('.userBubble') || host.querySelector('.msg'));
      const text = bubble ? bubble.innerText : '';
      navigator.clipboard.writeText(text).then(
        () => toast('已复制'),
        () => toast('复制失败', 'error')
      );
      return;
    }
    const btn = e.target.closest('.copyBtn');
    if (!btn) return;
    const pre = btn.closest('.codeBlock').querySelector('pre');
    navigator.clipboard.writeText(pre.textContent).then(
      () => {
        btn.textContent = '已复制';
        setTimeout(() => (btn.textContent = '复制'), 1200);
      },
      () => toast('复制失败', 'error')
    );
  });

  // Ctrl+Alt+N 新会话；Esc 停止运行中的任务
  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey && e.altKey && (e.key === 'n' || e.key === 'N')) {
      e.preventDefault();
      newSession();
      return;
    }
    if (e.key === 'Escape' && S.running && !$('.modalMask') && !$('.contextMenu')) {
      stopRun();
    }
  });

  // 运行中每秒刷新耗时
  setInterval(() => {
    if (S.running) updateStats();
  }, 1000);

  // ---------- audit log ----------
  //
  // 审计是全局合规记录（不绑定某个会话），用独立视图承载：类型筛选 + 刷新 + 自动轮询。
  // 后端 GET /api/audit 返回 {enabled, entries}，entries 已是新→旧。

  S.auditFilter = 'all';
  S.auditEntries = [];
  S.auditEnabled = false;
  S.auditLastTop = '';

  function auditIdentity(e) {
    return [e.ts, e.tool, e.session, e.action, e.blocked || '', e.dur_ms, e.result_len].join('|');
  }

  function auditTimeLabel(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return iso || '';
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  }

  function auditMetaItem(k, v) {
    const span = el('span');
    span.appendChild(el('b', null, k + '：'));
    span.appendChild(document.createTextNode(v));
    return span;
  }

  function auditRow(e) {
    const row = el('div', 'auditRow');
    row.dataset.action = e.action;

    const head = el('div', 'auditRowHead');
    const labels = { execute: '执行', denied: '拒绝', blocked: '拦截' };
    const act = el('span', 'auditAction', labels[e.action] || e.action);
    act.dataset.a = e.action;
    head.appendChild(act);
    head.appendChild(el('span', 'auditTool', e.tool || '—'));
    head.appendChild(el('span', 'auditTime', auditTimeLabel(e.ts)));
    if (e.action === 'execute') {
      const st = el('span', 'auditStatus', e.ok ? '✓ 成功' : '✗ 失败');
      st.dataset.ok = e.ok ? 'true' : 'false';
      head.appendChild(st);
    }
    row.appendChild(head);

    const meta = el('div', 'auditMeta');
    meta.appendChild(auditMetaItem('会话', e.session === '-' ? '本地' : e.session));
    if (e.node) meta.appendChild(auditMetaItem('节点', e.node));
    meta.appendChild(auditMetaItem('权限', e.perm || '—'));
    if (e.action === 'execute') {
      meta.appendChild(auditMetaItem('耗时', (e.dur_ms != null ? e.dur_ms : 0) + ' ms'));
      meta.appendChild(auditMetaItem('输出', fmtSize(e.result_len || 0)));
    }
    row.appendChild(meta);

    if (e.blocked) row.appendChild(el('div', 'auditReason', '原因：' + e.blocked));
    if (e.err) row.appendChild(el('div', 'auditErr', '错误：' + e.err));
    if (e.args) {
      const wrap = el('div', 'auditArgs');
      wrap.appendChild(el('div', 'auditArgsLabel', '参数（已脱敏）'));
      wrap.appendChild(el('pre', null, prettyArgs(e.args)));
      row.appendChild(wrap);
    }
    return row;
  }

  async function loadAudit() {
    try {
      const d = await api('/api/audit?n=500');
      S.auditEnabled = !!d.enabled;
      S.auditEntries = d.entries || [];
      S.auditLastTop = S.auditEntries.length ? auditIdentity(S.auditEntries[0]) : '';
      renderAudit();
    } catch (e) {
      const list = $('#auditList');
      list.innerHTML = '';
      list.appendChild(el('div', 'auditEmpty', '加载失败：' + e.message));
    }
  }

  function renderAudit() {
    const list = $('#auditList');
    list.innerHTML = '';
    const sub = $('#auditSub');
    if (!S.auditEnabled) {
      sub.textContent = '审计已关闭';
      sub.dataset.on = 'false';
    } else {
      sub.textContent = (S.auditEntries.length || '无') + ' 条记录';
      sub.dataset.on = 'true';
    }
    const f = S.auditFilter;
    const rows = S.auditEntries.filter((e) => f === 'all' || e.action === f);
    if (!rows.length) {
      list.appendChild(
        el('div', 'auditEmpty', S.auditEnabled ? '暂无该类型的审计记录' : '审计功能未开启（config 中 plugins_cfg.tools.audit=false 关闭了落盘）')
      );
      return;
    }
    rows.forEach((e) => list.appendChild(auditRow(e)));
  }

  function openAudit() {
    if (S.running) {
      toast('正在运行，先停止当前任务');
      return;
    }
    S.currentId = null;
    frame.dataset.view = 'audit';
    setDrawer(false);
    S.auditFilter = 'all';
    document.querySelectorAll('#auditFilter .auditFBtn').forEach((b) => {
      b.dataset.active = b.dataset.f === 'all' ? 'true' : 'false';
    });
    $('#convTitle').textContent = '操作审计日志';
    loadAudit();
  }

  // 仅当顶部记录变化才重渲染，避免无新记录时也重置已展开项。
  async function pollAudit() {
    try {
      const d = await api('/api/audit?n=500');
      S.auditEnabled = !!d.enabled;
      const entries = d.entries || [];
      const top = entries.length ? auditIdentity(entries[0]) : '';
      if (top !== S.auditLastTop) {
        S.auditEntries = entries;
        S.auditLastTop = top;
        renderAudit();
      }
    } catch {
      /* 轮询失败静默，不干扰使用 */
    }
  }

  $('#navAudit').onclick = openAudit;
  $('#auditRefresh').onclick = loadAudit;
  $('#auditFilter').addEventListener('click', (e) => {
    const b = e.target.closest('.auditFBtn');
    if (!b) return;
    S.auditFilter = b.dataset.f;
    document.querySelectorAll('#auditFilter .auditFBtn').forEach((x) => {
      x.dataset.active = x === b ? 'true' : 'false';
    });
    renderAudit();
  });

  // 审计视图打开且页面可见时，每 4s 轻量轮询顶部记录。
  setInterval(() => {
    if (frame.dataset.view === 'audit' && document.visibilityState === 'visible') pollAudit();
  }, 4000);

  // ---------- boot ----------

  // 从 URL hash 解析只读分享链接 #/share/<id>。命中返回 true。
  function routeFromHash() {
    const m = location.hash.match(/^#\/share\/(.+)$/);
    if (m) {
      loadShare(decodeURIComponent(m[1]));
      return true;
    }
    return false;
  }

  window.addEventListener('hashchange', () => {
    if (routeFromHash()) return;
    frame.classList.remove('shareMode');
  });

  (async function boot() {
    await refresh();
    autoGrow();
    if (routeFromHash()) {
      // 分享模式：不自动聚焦输入框
    } else if (S.sessions.length) {
      frame.dataset.view = 'empty';
    }
    if (!frame.classList.contains('shareMode')) input.focus();
  })();
})();
