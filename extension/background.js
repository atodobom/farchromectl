/* farchromectl — background service worker (MV3).
 *
 * Conecta ao servidor local (ws://127.0.0.1:<porta>/ws), autentica como
 * "ext", repassa comandos da UI às abas via chrome.tabs.sendMessage e
 * agrega os estados de mídia reportados pelos content scripts.
 *
 * Nota MV3: o service worker é suspenso pelo Chrome após inatividade.
 * Enquanto há mídia tocando, os pushes ~1s do content script e o tráfego do
 * WebSocket mantêm o SW vivo (Chrome 116+). Em idle longo o SW cai e
 * reconecta sozinho quando qualquer evento o acordar (mídia começa a tocar,
 * opções abertas etc.).
 */
"use strict";

// ---------- estado em memória do SW ----------
let ws = null;
let connecting = false;
let reconnectDelay = 1000;

let tabStates = new Map(); // tabId -> {title, playing, position, duration, volume, muted, playbackRate}
let targetTabId = null; // alvo fixado pelo usuário (setTargetTab)
let lastActiveTabId = null; // última aba detectada tocando
let pushTimer = null;

const RECONNECT_MIN = 1000;
const RECONNECT_MAX = 30000;
const PUSH_THROTTLE_MS = 1000;

// ---------- storage ----------
const DEFAULTS = { port: "8080", token: "" };

function getSettings() {
  return chrome.storage.local.get(DEFAULTS).then((s) => ({
    port: String(s.port || DEFAULTS.port),
    token: String(s.token || ""),
  }));
}

// ---------- conexão ----------
function serverURL(settings) {
  return "ws://127.0.0.1:" + settings.port + "/ws";
}

function ensureConnected() {
  if (ws || connecting) return;
  connecting = true;
  getSettings()
    .then((settings) => {
      return new Promise((resolve) => {
        let sock;
        try {
          sock = new WebSocket(serverURL(settings));
        } catch (e) {
          resolve(null);
          return;
        }
        let settled = false;
        const giveUp = setTimeout(() => {
          if (!settled) { settled = true; try { sock.close(); } catch (e) {} resolve(null); }
        }, 3000);
        sock.onopen = () => {
          clearTimeout(giveUp);
          sock.send(JSON.stringify({ role: "ext", token: settings.token }));
        };
        sock.onerror = () => { /* tratado em onclose */ };
        sock.onclose = () => {
          clearTimeout(giveUp);
          if (!settled) { settled = true; resolve(null); return; }
          // só contabiliza queda de conexão já estabelecida (ws === sock)
          if (ws === sock) onDisconnected();
        };
        sock.onmessage = (ev) => {
          let m;
          try { m = JSON.parse(ev.data); } catch (e) { return; }
          if (m.type === "hello") {
            if (m.ok) {
              settled = true;
              resolve(sock);
            } else {
              // token errado: espera maior antes de tentar de novo
              try { sock.close(); } catch (e) {}
              settled = true;
              resolve({ authFailed: true });
            }
          } else if (m.type === "cmd") {
            dispatchCommand(m).catch((e) => console.warn("farchromectl cmd:", e));
          }
        };
      });
    })
    .then((result) => {
      connecting = false;
      if (!result) { scheduleReconnect(); return; }
      if (result.authFailed) { reconnectDelay = 15000; scheduleReconnect(); return; }
      // caiu entre a autenticação e a atribuição: reconecta em vez de guardar socket morto
      if (result.readyState !== 1) { scheduleReconnect(); return; }
      ws = result;
      reconnectDelay = RECONNECT_MIN;
      requestFreshState(); // content scripts re-reportam (SW pode ter reiniciado)
      schedulePush(true); // estado assim que conecta
    });
}

// pede a todas as abas que reportem mídia novamente — usado após (re)conectar,
// porque o service worker MV3 pode ter sido suspenso e perdido o mapa de abas.
function requestFreshState() {
  chrome.tabs.query({}, (tabs) => {
    for (const t of tabs) {
      chrome.tabs.sendMessage(t.id, { cmd: "refreshState" }).catch(() => {});
    }
  });
}

function onDisconnected() {
  ws = null;
  scheduleReconnect();
}

function scheduleReconnect() {
  if (ws || connecting) return;
  const delay = Math.min(reconnectDelay, RECONNECT_MAX);
  reconnectDelay = Math.max(reconnectDelay * 2, RECONNECT_MIN);
  setTimeout(ensureConnected, delay);
}

function send(frame) {
  if (ws && ws.readyState === 1) {
    ws.send(JSON.stringify(frame));
  }
}

// ---------- despacho de comandos ----------
function resolveTargetTabId(frame) {
  if (typeof frame.tabId === "number") return frame.tabId;
  if (typeof frame.value === "number" && frame.cmd === "setTargetTab") return frame.value;
  if (typeof targetTabId === "number") return targetTabId;
  if (typeof lastActiveTabId === "number" && tabStates.has(lastActiveTabId)) return lastActiveTabId;
  if (tabStates.size === 1) return tabStates.keys().next().value;
  return null;
}

async function dispatchCommand(frame) {
  const cmd = frame.cmd;
  if (!cmd) return;

  if (cmd === "setTargetTab") {
    const id = resolveTargetTabId(frame);
    targetTabId = typeof id === "number" ? id : null;
    schedulePush(true);
    send({ type: "cmdResult", ok: true, cmd: cmd, tabId: targetTabId });
    return;
  }

  const tabId = resolveTargetTabId(frame);
  if (tabId === null) {
    send({ type: "cmdResult", ok: false, cmd: cmd, error: "nenhuma mídia detectada no Chrome" });
    return;
  }

  try {
    const resp = await chrome.tabs.sendMessage(tabId, { cmd: cmd, value: frame.value });
    if (resp && typeof resp.ok === "boolean") {
      send({ type: "cmdResult", ok: resp.ok, cmd: cmd, error: resp.error || "", tabId: tabId });
    } else {
      send({ type: "cmdResult", ok: false, cmd: cmd, error: "aba não respondeu", tabId: tabId });
    }
  } catch (e) {
    const msg = String(e && e.message || e);
    const hint = msg.includes("Could not establish connection")
      ? "aba sem content script (página interna do Chrome?) ou sem mídia"
      : msg;
    send({ type: "cmdResult", ok: false, cmd: cmd, error: hint, tabId: tabId });
  }
}

// ---------- agregação de estado ----------
function activeTabIdForPush() {
  if (typeof targetTabId === "number" && tabStates.has(targetTabId)) return targetTabId;
  if (typeof lastActiveTabId === "number" && tabStates.has(lastActiveTabId)) return lastActiveTabId;
  if (tabStates.size === 1) return tabStates.keys().next().value;
  return null;
}

function pushState() {
  pushTimer = null;
  if (!ws || ws.readyState !== 1) return;
  const tabs = [];
  for (const [id, st] of tabStates) {
    tabs.push(Object.assign({ tabId: id }, st));
  }
  send({ type: "state", tabs: tabs, activeTabId: activeTabIdForPush() });
}

function schedulePush(immediate) {
  if (immediate) {
    if (pushTimer) { clearTimeout(pushTimer); pushTimer = null; }
    pushState();
    return;
  }
  if (pushTimer) return;
  pushTimer = setTimeout(pushState, PUSH_THROTTLE_MS);
}

// ---------- eventos ----------
chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (!msg || msg.type !== "tabState" || !sender.tab) {
    sendResponse({ ok: false, error: "quadro desconhecido" });
    return;
  }
  const tabId = sender.tab.id;
  const hadMedia = tabStates.has(tabId);
  const prev = tabStates.get(tabId) || {};
  const hasMedia = !!(msg.hasMedia && msg.state);
  let playingNow = false;
  if (hasMedia) {
    playingNow = !!msg.state.playing;
    tabStates.set(tabId, msg.state);
    if (playingNow) lastActiveTabId = tabId;
  } else {
    tabStates.delete(tabId);
    if (targetTabId === tabId) targetTabId = null;
    if (lastActiveTabId === tabId) lastActiveTabId = null;
  }
  // push imediato quando a mídia aparece/desaparece ou o play/pause muda;
  // otherwise entra no throttle de ~1s
  const changed = hadMedia !== hasMedia || !!prev.playing !== playingNow;
  schedulePush(changed);
  sendResponse({ ok: true });
});

chrome.tabs.onRemoved.addListener((tabId) => {
  if (tabStates.delete(tabId)) {
    if (targetTabId === tabId) targetTabId = null;
    if (lastActiveTabId === tabId) lastActiveTabId = null;
    schedulePush(true);
  }
});

chrome.storage.onChanged.addListener((changes, area) => {
  if (area !== "local") return;
  if (changes.port || changes.token) {
    reconnectDelay = RECONNECT_MIN;
    if (ws) { try { ws.close(); } catch (e) {} ws = null; }
    connecting = false;
    ensureConnected();
  }
});

chrome.runtime.onStartup.addListener(() => { ensureConnected(); });

// SW (re)iniciado: conecta imediatamente.
ensureConnected();
