/* farchromectl — content script.
 *
 * Detecta elementos <video>/<audio> na página, escolhe o "melhor"
 * (tocando > maior área > maior duração), reporta o estado ao background
 * (throttle ~1s enquanto toca; imediato em mudanças) e aplica os comandos
 * recebidos (play/pause/seek/volume/mute/velocidade).
 *
 * Limitações conhecidas (propositalmente sem fake):
 *  - DRM/streams protegidos: seek/volume podem não ter efeito — o erro é
 *    reportado ao invés de engolido;
 *  - next/prev-track não é possível genericamente (handlers Media Session
 *    dos sites não podem ser disparados externamente).
 */
"use strict";

(function () {
  const REPORT_THROTTLE_MS = 1000;
  const REPORT_MAX_MS = 500; // atraso máximo para eventos "importantes"

  let lastTimeupdateReport = 0;
  let reportTimer = null;
  let mutationTimer = null;

  // ---------- seleção do melhor elemento ----------
  function mediaElements() {
    return Array.from(document.querySelectorAll("video, audio"));
  }

  function score(el) {
    const area = (el.videoWidth || el.clientWidth || 0) * (el.videoHeight || el.clientHeight || 0);
    const dur = isFinite(el.duration) && el.duration > 0 ? el.duration : 0;
    const playing = !el.paused && !el.ended ? 1 : 0;
    // tocando >> área >> duração
    return playing * 1e12 + area * 1e3 + dur;
  }

  function bestElement() {
    const els = mediaElements();
    if (els.length === 0) return null;
    let best = null;
    let bestScore = -1;
    for (const el of els) {
      const s = score(el);
      if (s > bestScore) {
        bestScore = s;
        best = el;
      }
    }
    return best;
  }

  function hasUsableMedia() {
    return mediaElements().length > 0;
  }

  // ---------- estado ----------
  function finiteOr(n, fallback) {
    return typeof n === "number" && isFinite(n) ? n : fallback;
  }

  function buildState(el) {
    const meta = navigator.mediaSession && navigator.mediaSession.metadata;
    return {
      title: (meta && meta.title) || document.title || "",
      playing: !!el && !el.paused && !el.ended,
      position: el ? finiteOr(el.currentTime, 0) : 0,
      duration: el ? finiteOr(el.duration, null) : null, // streams ao vivo: null
      volume: el ? finiteOr(el.volume, 1) : 1,
      muted: !!el && el.muted,
      playbackRate: el ? finiteOr(el.playbackRate, 1) : 1,
    };
  }

  function report() {
    const el = bestElement();
    const msg = {
      type: "tabState",
      hasMedia: hasUsableMedia(),
      state: el ? buildState(el) : null,
    };
    try {
      const p = chrome.runtime.sendMessage(msg);
      if (p && p.catch) p.catch(() => { /* SW suspenso: próximo evento reacorda */ });
    } catch (e) { /* contexto de extensão invalidado */ }
  }

  function scheduleReport(delayMs) {
    if (reportTimer) clearTimeout(reportTimer);
    reportTimer = setTimeout(() => {
      reportTimer = null;
      report();
    }, delayMs);
  }

  // ---------- aplicação de comandos ----------
  function clamp(t, el) {
    if (t < 0) return 0;
    if (isFinite(el.duration) && el.duration > 0 && t > el.duration) return el.duration;
    return t;
  }

  // applyCommand devolve SEMPRE uma Promise<{ok, error?}>. play() é assíncrono
  // e pode ser bloqueado pela política de autoplay — o resultado real é
  // reportado ao background.
  function applyCommand(cmd, value) {
    return new Promise((resolve) => {
      const el = bestElement();
      if (!el) {
        resolve({ ok: false, error: "nenhum elemento de mídia na página" });
        return;
      }
      const playResult = (p) =>
        p.then(
          () => resolve({ ok: true }),
          (e) => resolve({ ok: false, error: "reprodução bloqueada: " + ((e && e.message) || e) })
        );
      try {
        switch (cmd) {
          case "play":
            playResult(el.play());
            return;
          case "pause":
            el.pause();
            break;
          case "toggle":
            if (el.paused || el.ended) {
              playResult(el.play());
              return;
            }
            el.pause();
            break;
          case "seekBack":
            el.currentTime = clamp(el.currentTime - 10, el);
            break;
          case "seekForward":
            el.currentTime = clamp(el.currentTime + 10, el);
            break;
          case "seekTo":
            if (typeof value !== "number" || !isFinite(value)) {
              resolve({ ok: false, error: "seekTo exige valor numérico" });
              return;
            }
            el.currentTime = clamp(value, el);
            break;
          case "setVolume":
            if (typeof value !== "number" || value < 0 || value > 1) {
              resolve({ ok: false, error: "volume deve estar entre 0 e 1" });
              return;
            }
            el.volume = value;
            if (value > 0 && el.muted) el.muted = false;
            break;
          case "mute":
            el.muted = true;
            break;
          case "unmute":
            el.muted = false;
            break;
          case "setPlaybackRate":
            if (typeof value !== "number" || value <= 0) {
              resolve({ ok: false, error: "playbackRate deve ser positivo" });
              return;
            }
            el.playbackRate = value;
            break;
          case "refreshState":
            // pedido do background após (re)conectar: o listener já agenda o
            // report; nada a aplicar aqui.
            break;
          default:
            resolve({ ok: false, error: "comando desconhecido: " + cmd });
            return;
        }
      } catch (e) {
        // DRM / streams protegidos podem lançar ao tocar em currentTime etc.
        resolve({ ok: false, error: "falha ao aplicar " + cmd + ": " + ((e && e.message) || e) });
        return;
      }
      resolve({ ok: true });
    });
  }

  chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
    if (!msg || typeof msg.cmd !== "string") {
      sendResponse({ ok: false, error: "comando ausente" });
      return;
    }
    applyCommand(msg.cmd, msg.value).then((r) => {
      scheduleReport(0); // confirma o novo estado o quanto antes
      sendResponse(r);
    });
    return true; // resposta assíncrona (MV3)
  });

  // ---------- eventos de mídia ----------
  const MEDIA_EVENTS = ["play", "pause", "ended", "seeked", "volumechange", "ratechange", "loadedmetadata"];
  for (const evName of MEDIA_EVENTS) {
    document.addEventListener(evName, () => scheduleReport(0), true);
  }
  document.addEventListener("timeupdate", () => {
    const now = Date.now();
    if (now - lastTimeupdateReport < REPORT_THROTTLE_MS) return;
    lastTimeupdateReport = now;
    scheduleReport(REPORT_MAX_MS);
  }, true);

  // mídias que aparecem/somem depois (SPA, lazy load)
  const obs = new MutationObserver(() => {
    if (mutationTimer) clearTimeout(mutationTimer);
    mutationTimer = setTimeout(() => {
      mutationTimer = null;
      scheduleReport(0);
    }, 250);
  });
  obs.observe(document.documentElement, { childList: true, subtree: true });

  // estado inicial
  scheduleReport(0);
})();
