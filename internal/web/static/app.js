/* gsh webui */
const $ = (sel) => document.querySelector(sel);
const chat = $("#chat");
const input = $("#input");
const sendBtn = $("#send");
const statusDot = $("#statusDot");
const sessionList = $("#sessionList");
const sessionTitle = $("#sessionTitle");

let sessions = [];
let currentId = null;
let running = false;

/* ---------- session list ---------- */
async function refreshSessions() {
  const r = await fetch("/api/sessions");
  const data = await r.json();
  sessions = data.sessions || [];
  renderSessionList();
}

function renderSessionList() {
  sessionList.innerHTML = "";
  for (const id of sessions) {
    const el = document.createElement("div");
    el.className = "session-item" + (id === currentId ? " active" : "");
    el.textContent = id;
    el.title = id;
    el.onclick = () => openSession(id);
    sessionList.appendChild(el);
  }
}

async function openSession(id) {
  currentId = id;
  sessionTitle.textContent = id;
  renderSessionList();
  const r = await fetch("/api/session?id=" + encodeURIComponent(id));
  const data = await r.json();
  chat.innerHTML = "";
  for (const m of data.messages || []) {
    if (m.role === "user") addMsg("user", m.content);
    else if (m.role === "assistant" && m.content) addMsg("assistant", m.content);
    else if (m.role === "tool") addToolCard(m.name || "tool", "", m.content, true);
  }
  scrollBottom();
}

/* ---------- chat rendering ---------- */
function addMsg(role, text) {
  const div = document.createElement("div");
  div.className = "msg " + role;
  div.innerHTML = `<div class="role">${role}</div><div class="bubble"></div>`;
  div.querySelector(".bubble").textContent = text;
  chat.appendChild(div);
  scrollBottom();
  return div.querySelector(".bubble");
}

function addToolCard(name, args, result, collapsed) {
  const div = document.createElement("div");
  div.className = "tool" + (collapsed ? "" : " open");
  const head = document.createElement("div");
  head.className = "tool-head";
  head.innerHTML = `<span class="icon">▶</span><span class="tname"></span><span class="summary"></span>`;
  head.querySelector(".tname").textContent = name;
  const body = document.createElement("div");
  body.className = "tool-body";
  if (args) {
    const lbl = document.createElement("div");
    lbl.className = "label";
    lbl.textContent = "args";
    const pre = document.createElement("div");
    pre.textContent = args;
    body.appendChild(lbl);
    body.appendChild(pre);
  }
  if (result) {
    const lbl = document.createElement("div");
    lbl.className = "label";
    lbl.textContent = "result";
    const pre = document.createElement("div");
    pre.textContent = result;
    body.appendChild(lbl);
    body.appendChild(pre);
  }
  head.onclick = () => div.classList.toggle("open");
  div.appendChild(head);
  div.appendChild(body);
  chat.appendChild(div);
  scrollBottom();
  return { body, summary: head.querySelector(".summary") };
}

function scrollBottom() {
  chat.scrollTop = chat.scrollHeight;
}

function setRunning(v) {
  running = v;
  sendBtn.disabled = v;
  statusDot.className = "dot " + (v ? "running" : "idle");
}

/* ---------- send (SSE) ---------- */
async function send() {
  const prompt = input.value.trim();
  if (!prompt || running) return;
  input.value = "";
  autoGrow();

  if (!currentId) sessionTitle.textContent = "新会话";
  addMsg("user", prompt);
  setRunning(true);

  let bubble = null;
  let cursor = null;

  try {
    const resp = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt }),
    });
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        handleSSE(chunk);
      }
    }
  } catch (e) {
    statusDot.className = "dot error";
    addMsg("assistant", "[connection error] " + e.message);
  } finally {
    if (cursor) cursor.remove();
    setRunning(false);
    refreshSessions();
  }

  function handleSSE(chunk) {
    let event = "message", data = "";
    for (const line of chunk.split("\n")) {
      if (line.startsWith("event: ")) event = line.slice(7).trim();
      else if (line.startsWith("data: ")) data += line.slice(6);
    }
    if (!data) return;
    let payload;
    try { payload = JSON.parse(data); } catch { return; }

    switch (event) {
      case "delta":
        if (!bubble) {
          bubble = addMsg("assistant", "");
          cursor = document.createElement("span");
          cursor.className = "cursor";
          bubble.appendChild(cursor);
        }
        cursor.insertAdjacentText("beforebegin", payload.text);
        scrollBottom();
        break;
      case "tool_call":
        if (cursor) { cursor.remove(); cursor = null; }
        bubble = null;
        addToolCard(payload.name || "tool", payload.args || "", "", false);
        break;
      case "tool_result": {
        const cards = chat.querySelectorAll(".tool");
        const last = cards[cards.length - 1];
        if (last && !last.dataset.done) {
          last.dataset.done = "1";
          const body = last.querySelector(".tool-body");
          const lbl = document.createElement("div");
          lbl.className = "label";
          lbl.textContent = "result";
          const pre = document.createElement("div");
          pre.textContent = payload.data || "";
          body.appendChild(lbl);
          body.appendChild(pre);
          last.querySelector(".summary").textContent = " ✓";
        }
        scrollBottom();
        break;
      }
      case "final":
        if (cursor) { cursor.remove(); cursor = null; }
        if (!bubble && payload.data) addMsg("assistant", payload.data);
        break;
      case "error":
        if (cursor) { cursor.remove(); cursor = null; }
        statusDot.className = "dot error";
        addMsg("assistant", "[error] " + (payload.message || "unknown"));
        break;
      case "done":
        if (cursor) { cursor.remove(); cursor = null; }
        break;
    }
  }
}

/* ---------- composer ---------- */
function autoGrow() {
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 180) + "px";
}
input.addEventListener("input", autoGrow);
input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    send();
  }
});
sendBtn.addEventListener("click", send);

/* ---------- boot ---------- */
(async function boot() {
  // provider 标签：通过一次空探测得知（简化：读 echo 特征，正式做法是 /api/meta）
  $("#providerTag").textContent = "kernel: 5 plugins";
  await refreshSessions();
  if (sessions.length > 0) openSession(sessions[0]);
  else {
    chat.innerHTML = `<div class="empty"><div class="star">✦</div><p>一切皆插件 · 输入任务开始</p></div>`;
  }
  input.focus();
})();
