/* farchromectl — página de opções da extensão (chrome.storage.local). */
"use strict";

(function () {
  const DEFAULTS = { port: "8080", token: "" };

  const elPort = document.getElementById("opt-port");
  const elToken = document.getElementById("opt-token");
  const elStatus = document.getElementById("opt-status");
  const elSave = document.getElementById("opt-save");

  chrome.storage.local.get(DEFAULTS).then((s) => {
    elPort.value = String(s.port || DEFAULTS.port);
    elToken.value = String(s.token || "");
  });

  elSave.addEventListener("click", () => {
    const port = elPort.value.trim();
    const token = elToken.value.trim();

    if (!/^[0-9]{1,5}$/.test(port) || Number(port) < 1 || Number(port) > 65535) {
      elStatus.textContent = "Porta inválida (use, por exemplo, 8080).";
      elStatus.style.color = "#e05260";
      return;
    }
    if (!/^[0-9]{6}$/.test(token)) {
      elStatus.textContent = "Token inválido — são 6 dígitos, exibidos no terminal do servidor.";
      elStatus.style.color = "#e05260";
      return;
    }

    chrome.storage.local.set({ port: port, token: token }).then(() => {
      // background.js escuta chrome.storage.onChanged e reconecta sozinho.
      elStatus.textContent = "Salvo — reconectando…";
      elStatus.style.color = "";
    });
  });
})();
