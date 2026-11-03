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
  };

  const frame = $('#frame');
  const chatEl = $('#chat');
  const chatScroll = $('#chatScroll');
  const input = $('#input');
  const sendBtn = $('#sendBtn');

  // ---------- helpers ----------

  const pad = (n) => String(n).padStart(2, '0');

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
    const name = el('span', 'wsName', S.workspace || '默认工作区');
    ws.appendChild(name);
    ws.title = S.workspace || '';
    group.appendChild(ws);

    S.sessions.slice(0, 12).forEach((s) => {
      const item = el('button', 'convItem');
      item.dataset.id = s.id;
      if (s.id === S.currentId) item.dataset.active = 'true';
      item.appendChild(el('span', 'convName', s.title || '新会话'));
      if (s.updated) item.appendChild(el('span', 'convAge', ageLabel(s.updated)));
      item.onclick = () => openSession(s.id);
      item.oncontextmenu = (e) => {
        e.preventDefault();
        if (confirm(`删除会话「${s.title || '新会话'}」？此操作不可恢复。`)) deleteSession(s.id);
      };
      group.appendChild(item);
    });

    list.appendChild(group);
    $('#wsPickerLabel').textContent = S.workspace || '默认工作区';
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

  function appendUser(content, iso) {
    const t = newTurn();
    const row = el('div', 'userRow');
    row.appendChild(el('div', 'userBubble', content));
    if (iso) row.appendChild(el('div', 'userMeta', timeLabel(iso)));
    t.root.appendChild(row);
    return t;
  }

  function ensureAssistant(t) {
    if (!t.msgEl) {
      t.msgEl = el('div', 'msg');
      t.root.appendChild(t.msgEl);
    }
    return t.msgEl;
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
      S.sessions = st.sessions || [];
      $('#modelName').textContent = S.model || '未配置模型';
      renderSidebar();
    } catch (e) {
      toast('加载失败：' + e.message, 'error');
    }
  }

  // ---------- send ----------

  async function send() {
    const text = input.value.trim();
    if (!text || S.running) return;

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
    input.value = '';
    autoGrow();
    setRunning(true);

    const t = appendUser(text, new Date().toISOString());
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
        body: JSON.stringify({ prompt: text, session_id: S.currentId, run_id: runId }),
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

  $('#newSessionBtn').onclick = newSession;

  $('#collapseBtn').onclick = () => {
    frame.dataset.animating = '1';
    frame.dataset.sidebar = frame.dataset.sidebar === 'collapsed' ? 'expanded' : 'collapsed';
    setTimeout(() => delete frame.dataset.animating, 320);
  };

  $('#modelBtn').onclick = openSettings;
  $('#permBtn').onclick = () =>
    toast('当前权限：完全权限（allow_destructive 由配置决定）');
  $('#attachBtn').onclick = () => toast('附件上传待接入');
  $('#addWorkspace').onclick = () => toast('工作区由 config.json 的 plugins_cfg.tools.workspace 配置');
  $('#wsPicker').onclick = () => toast('工作区由 config.json 的 plugins_cfg.tools.workspace 配置');
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
  $('#btnRight').onclick = () => toast('右侧面板待接入');

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

  // 代码块复制（事件委托）
  chatEl.addEventListener('click', (e) => {
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

  // Ctrl+Alt+N 新会话
  document.addEventListener('keydown', (e) => {
    if (e.ctrlKey && e.altKey && (e.key === 'n' || e.key === 'N')) {
      e.preventDefault();
      newSession();
    }
  });

  // 运行中每秒刷新耗时
  setInterval(() => {
    if (S.running) updateStats();
  }, 1000);

  // ---------- boot ----------

  (async function boot() {
    await refresh();
    autoGrow();
    if (S.sessions.length) {
      frame.dataset.view = 'empty';
    }
    input.focus();
  })();
})();
