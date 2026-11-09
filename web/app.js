/* farchromectl — UI (vanilla JS, sem build step).
 *
 * Fluxo:
 *  1. Descoberta do servidor: (a) último baseUrl salvo; (b) nome mDNS
 *     farchromectl.local:<porta salva ou 8080>; (c) formulário manual.
 *  2. Autenticação via token no WebSocket; credenciais salvas em
 *     localStorage e reutilizadas até falharem.
 *  3. Relay de comandos/estado conforme o protocolo do servidor.
 */
"use strict";

(function () {
  var STORAGE_KEY = "farchromectl";
  var DEFAULT_PORT = "8080";
  var PROBE_TIMEOUT_MS = 2000;
  var RECONNECT_MIN_MS = 2000;
  var RECONNECT_MAX_MS = 10000;
  var SPEEDS = [1, 1.25, 1.5, 2];

  // ---------- estado ----------
  var creds = loadCreds(); // {baseUrl, token} | null
  var ws = null;
  var serverOk = false;
  var extOk = false;
  var authFailedOnce = false; // re-prompt de token apenas uma vez por sessão
  var tabs = []; // [{tabId,title,playing,...}]
  var activeTabId = null;
  var chosenTabId = null; // alvo escolhido manualmente no seletor
  var scrubbing = false;
  var reconnectTimer = null;
  var reconnectDelay = RECONNECT_MIN_MS;
  var toastTimer = null;

  // ---------- helpers DOM ----------
  function $(id) { return document.getElementById(id); }

  var el = {
    connect: $("connect"), status: $("connect-status"),
    form: $("connect-form"), fUrl: $("f-url"), fToken: $("f-token"),
    hint: $("connect-hint"),
    main: $("main"),
    dotServer: $("dot-server"), dotExt: $("dot-ext"),
    bannerExt: $("banner-ext"), toast: $("toast"),
    npTitle: $("np-title"), npState: $("np-state"),
    pos: $("pos"), dur: $("dur"), scrub: $("scrub"),
    targetRow: $("target-row"), target: $("target"),
    btnBack: $("btn-back"), btnToggle: $("btn-toggle"), btnFwd: $("btn-fwd"),
    btnMute: $("btn-mute"), volume: $("volume"), btnSpeed: $("btn-speed"),
    btnSettings: $("btn-settings"), settings: $("settings"),
    settingsInfo: $("settings-info"),
    btnForget: $("btn-forget"), btnCloseSettings: $("btn-close-settings"),
  };

  // ---------- armazenamento ----------
  function loadCreds() {
    try {
      var raw = localStorage.getItem(STORAGE_KEY);
      if (!raw) return null;
      var c = JSON.parse(raw);
      if (typeof c.baseUrl !== "string" || !c.baseUrl) return null;
      c.token = typeof c.token === "string" ? c.token : "";
      return c;
    } catch (e) {
      return null;
    }
  }
  function saveCreds(baseUrl, token) {
    creds = { baseUrl: baseUrl, token: token };
    localStorage.setItem(STORAGE_KEY, JSON.stringify(creds));
  }
  function forgetCreds() {
    localStorage.removeItem(STORAGE_KEY);
    creds = null;
  }

  // ---------- descoberta ----------
  function portOf(baseUrl) {
    try {
      var u = new URL(baseUrl);
      return u.port || (u.protocol === "https:" ? "443" : "80");
    } catch (e) {
      return DEFAULT_PORT;
    }
  }

  function candidates() {
    var list = [];
    if (creds && creds.baseUrl) list.push(creds.baseUrl);
    var p = creds && creds.baseUrl ? portOf(creds.baseUrl) : DEFAULT_PORT;
    var mdns = "http://farchromectl.local:" + (p || DEFAULT_PORT);
    if (list.indexOf(mdns) === -1) list.push(mdns);
    return list;
  }

  // probe via GET /api/healthz com timeout curto
  function probe(baseUrl) {
    return new Promise(function (resolve) {
      var ctrl = new AbortController();
      var t = setTimeout(function () { ctrl.abort(); }, PROBE_TIMEOUT_MS);
      fetch(baseUrl.replace(/\/+$/, "") + "/api/healthz", { signal: ctrl.signal, cache: "no-store" })
        .then(function (r) { return r.json(); })
        .then(function (j) {
          clearTimeout(t);
          resolve(j && j.ok === true && j.name === "farchromectl" ? baseUrl : null);
        })
        .catch(function () { clearTimeout(t); resolve(null); });
    });
  }

  function probeAll(list) {
    return list.reduce(
      function (chain, url) {
        return chain.then(function (found) {
          if (found) return found;
          return probe(url);
        });
      },
      Promise.resolve(null)
    );
  }

  // ---------- fluxo de boot ----------
  function boot() {
    setStatus("Procurando servidor na rede…");
    probeAll(candidates()).then(function (found) {
      if (found) {
        if (creds && creds.token) {
          connect(found, creds.token);
        } else {
          showForm(found, "");
        }
      } else {
        // nenhum candidato respondeu: formulário manual com palpite
        showForm((creds && creds.baseUrl) || "http://farchromectl.local:" + DEFAULT_PORT,
                 (creds && creds.token) || "");
        setStatus("");
        showHint("Servidor não encontrado automaticamente. Confira o endereço, a rede Wi-Fi e o firewall.", false);
      }
    });
  }

  function setStatus(msg) { el.status.textContent = msg; }

  function showHint(msg, isError) {
    el.hint.textContent = msg;
    el.hint.hidden = !msg;
    el.hint.className = "hint" + (isError ? " hint-error" : "");
  }

  function showForm(urlGuess, tokenGuess) {
    el.form.hidden = false;
    el.fUrl.value = urlGuess || "";
    el.fToken.value = tokenGuess || "";
    el.fToken.focus();
  }

  el.form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var url = el.fUrl.value.trim().replace(/\/+$/, "");
    var token = el.fToken.value.trim();
    if (!/^https?:\/\/.+/.test(url)) {
      showHint("O endereço deve começar com http:// e incluir a porta (ex.: http://192.168.0.10:8080).", true);
      return;
    }
    if (!/^[0-9]{6}$/.test(token)) {
      showHint("O token tem 6 dígitos — ele aparece no terminal quando o servidor inicia.", true);
      return;
    }
    showHint("", false);
    setStatus("Conectando a " + url + "…");
    probe(url).then(function (found) {
      if (!found) {
        showHint("Nenhum servidor farchromectl em " + url + ".", true);
        setStatus("");
        return;
      }
      connect(found, token);
    });
  });

  // ---------- WebSocket ----------
  function wsURL(baseUrl) {
    return baseUrl.replace(/^http/i, "ws") + "/ws";
  }

  function connect(baseUrl, token) {
    setStatus("Conectando…");
    try {
      ws = new WebSocket(wsURL(baseUrl));
    } catch (e) {
      scheduleReconnect();
      return;
    }

    ws.onopen = function () {
      ws.send(JSON.stringify({ role: "ui", token: token }));
    };

    ws.onmessage = function (ev) {
      var m;
      try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (m.type === "hello") {
        if (m.ok) {
          serverOk = true;
          reconnectDelay = RECONNECT_MIN_MS;
          saveCreds(baseUrl, token);
          enterMain();
        } else {
          // token errado: limpa e re-pede UMA vez com dica
          ws.close();
          ws = null;
          var badToken = token;
          forgetTokenOnly();
          if (!authFailedOnce) {
            authFailedOnce = true;
            showForm(baseUrl, "");
            setStatus("");
            showHint("Token errado (" + badToken + "). Digite o token atual — ele aparece no terminal do servidor.", true);
          } else {
            setStatus("Token recusado. Verifique o token no terminal do servidor.");
            scheduleReconnect();
          }
        }
        return;
      }
      handleFrame(m);
    };

    ws.onclose = function () {
      var wasOk = serverOk;
      serverOk = false;
      updateDots();
      if (ws) { ws = null; }
      if (wasOk || !authFailedOnce) {
        scheduleReconnect();
      }
    };
    ws.onerror = function () { /* onclose cuida da reconexão */ };
  }

  function forgetTokenOnly() {
    if (creds) {
      creds.token = "";
      localStorage.setItem(STORAGE_KEY, JSON.stringify(creds));
    }
  }

  function scheduleReconnect() {
    if (reconnectTimer) return;
    setStatus("Reconectando…");
    reconnectTimer = setTimeout(function () {
      reconnectTimer = null;
      boot();
    }, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
  }

  function enterMain() {
    el.connect.hidden = true;
    el.main.hidden = false;
    updateDots();
    el.settingsInfo.textContent =
      "Servidor: " + (creds ? creds.baseUrl : "?") + " — Token: " + (creds ? creds.token : "?");
  }

  // ---------- frames do servidor ----------
  function handleFrame(m) {
    switch (m.type) {
      case "extStatus":
        extOk = !!m.connected;
        updateDots();
        break;
      case "state":
        tabs = Array.isArray(m.tabs) ? m.tabs : [];
        activeTabId = typeof m.activeTabId === "number" ? m.activeTabId : null;
        if (chosenTabId !== null && !tabById(chosenTabId)) chosenTabId = null;
        renderState();
        break;
      case "cmdResult":
        if (m.ok === false) toast(m.error || "Comando falhou");
        break;
    }
  }

  function tabById(id) {
    for (var i = 0; i < tabs.length; i++) if (tabs[i].tabId === id) return tabs[i];
    return null;
  }

  function currentTab() {
    if (chosenTabId !== null) {
      var t = tabById(chosenTabId);
      if (t) return t;
    }
    if (activeTabId !== null) return tabById(activeTabId);
    if (tabs.length === 1) return tabs[0];
    return null;
  }

  // ---------- envio de comandos ----------
  function sendCmd(cmd, value) {
    if (!ws || ws.readyState !== 1) return;
    var frame = { type: "cmd", cmd: cmd };
    if (typeof value === "number") frame.value = value;
    if (chosenTabId !== null) frame.tabId = chosenTabId;
    ws.send(JSON.stringify(frame));
  }

  // ---------- renderização ----------
  function fmtTime(s) {
    if (typeof s !== "number" || !isFinite(s) || s < 0) return "0:00";
    var m = Math.floor(s / 60);
    var sec = Math.floor(s % 60);
    return m + ":" + (sec < 10 ? "0" : "") + sec;
  }

  function updateDots() {
    el.dotServer.classList.toggle("on", serverOk);
    el.dotExt.classList.toggle("on", extOk);
    el.bannerExt.hidden = extOk;
  }

  function renderState() {
    var t = currentTab();
    if (!t) {
      el.npTitle.textContent = tabs.length ? "Escolha uma aba" : "Nada tocando";
      el.npState.textContent = "—";
      el.pos.textContent = "0:00";
      el.dur.textContent = "0:00";
      el.btnToggle.textContent = "▶";
      el.targetRow.hidden = tabs.length <= 1;
      renderTargetOptions();
      return;
    }

    el.npTitle.textContent = t.title || "Sem título";
    el.npState.textContent = t.playing ? "Reproduzindo" : "Pausado";
    el.btnToggle.textContent = t.playing ? "⏸" : "▶";
    el.btnMute.textContent = t.muted || (t.volume === 0) ? "🔇" : "🔊";
    el.btnSpeed.textContent = (SPEEDS.indexOf(t.playbackRate) !== -1 ? t.playbackRate : 1) + "×";

    if (typeof t.volume === "number" && document.activeElement !== el.volume) {
      el.volume.value = Math.round(t.volume * 100);
    }
    el.dur.textContent = fmtTime(t.duration);
    if (!scrubbing) {
      var frac = typeof t.duration === "number" && t.duration > 0 ? t.position / t.duration : 0;
      if (document.activeElement !== el.scrub) {
        el.scrub.value = Math.round(Math.min(Math.max(frac, 0), 1) * 1000);
        el.pos.textContent = fmtTime(t.position);
      }
    }

    el.targetRow.hidden = tabs.length <= 1;
    renderTargetOptions();
  }

  function renderTargetOptions() {
    var want = chosenTabId !== null ? chosenTabId : activeTabId;
    // só recria as options quando a lista mudar (evita perder o foco do select)
    var sig = tabs.map(function (t) { return t.tabId + ":" + (t.title || ""); }).join("|") + "#" + want;
    if (renderTargetOptions._sig === sig) return;
    renderTargetOptions._sig = sig;

    while (el.target.firstChild) el.target.removeChild(el.target.firstChild);
    tabs.forEach(function (t) {
      var o = document.createElement("option");
      o.value = String(t.tabId);
      o.textContent = (t.title || "Aba " + t.tabId).slice(0, 60) + (t.playing ? " ▶" : "");
      if (t.tabId === want) o.selected = true;
      el.target.appendChild(o);
    });
  }

  // ---------- controles ----------
  el.btnToggle.addEventListener("click", function () { sendCmd("toggle"); });
  el.btnBack.addEventListener("click", function () { sendCmd("seekBack"); });
  el.btnFwd.addEventListener("click", function () { sendCmd("seekForward"); });
  el.btnMute.addEventListener("click", function () {
    var t = currentTab();
    sendCmd(t && (t.muted || t.volume === 0) ? "unmute" : "mute");
  });

  el.volume.addEventListener("input", function () {
    sendCmd("setVolume", Number(el.volume.value) / 100);
  });

  el.btnSpeed.addEventListener("click", function () {
    var t = currentTab();
    var cur = t && SPEEDS.indexOf(t.playbackRate) !== -1 ? t.playbackRate : 1;
    var next = SPEEDS[(SPEEDS.indexOf(cur) + 1) % SPEEDS.length];
    sendCmd("setPlaybackRate", next);
  });

  el.scrub.addEventListener("pointerdown", function () { scrubbing = true; });
  el.scrub.addEventListener("input", function () {
    var t = currentTab();
    var dur = t && typeof t.duration === "number" ? t.duration : 0;
    el.pos.textContent = fmtTime(dur * Number(el.scrub.value) / 1000);
  });
  function commitScrub() {
    if (!scrubbing) return;
    scrubbing = false;
    var t = currentTab();
    if (!t || typeof t.duration !== "number" || t.duration <= 0) return;
    sendCmd("seekTo", t.duration * Number(el.scrub.value) / 1000);
  }
  el.scrub.addEventListener("pointerup", commitScrub);
  el.scrub.addEventListener("pointercancel", function () { scrubbing = false; });
  el.scrub.addEventListener("keyup", commitScrub); // acessibilidade: setas no slider

  el.target.addEventListener("change", function () {
    chosenTabId = Number(el.target.value);
    sendCmd("setTargetTab"); // sendCmd anexa o tabId escolhido automaticamente
    renderState();
  });

  // ---------- ajustes ----------
  el.btnSettings.addEventListener("click", function () {
    if (typeof el.settings.showModal === "function") el.settings.showModal();
    else el.settings.setAttribute("open", "");
  });
  el.btnCloseSettings.addEventListener("click", function () { el.settings.close(); });
  el.btnForget.addEventListener("click", function () {
    forgetCreds();
    if (ws) { try { ws.close(); } catch (e) {} }
    ws = null;
    serverOk = false; extOk = false; authFailedOnce = true; // não reconecta sozinho
    el.settings.close();
    el.main.hidden = true;
    el.connect.hidden = false;
    setStatus("");
    showForm("", "");
    showHint("Credenciais apagadas.", false);
  });

  // ---------- toast ----------
  function toast(msg) {
    el.toast.textContent = msg;
    el.toast.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.toast.hidden = true; }, 3000);
  }

  // ---------- start ----------
  updateDots();
  boot();
})();
