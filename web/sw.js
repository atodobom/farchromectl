/* farchromectl — service worker (cache do app shell para PWA/offline).
 *
 * Estratégia network-first: em uso normal o conteúdo vem sempre da rede
 * (a UI nunca fica velha); o cache só serve quando a rede falha — celular
 * fora do alcance do servidor, por exemplo.
 *
 * O registro (em app.js) acontece somente em contexto seguro: HTTPS
 * (modo --tls do servidor) ou localhost. Em HTTP puro pela LAN não há
 * tentativa de registro — o comportamento é idêntico ao de antes do SW.
 */
"use strict";

var CACHE = "farchromectl-v1";
var SHELL = [
  "/",
  "/index.html",
  "/app.js",
  "/style.css",
  "/manifest.webmanifest",
  "/icon.svg"
];

self.addEventListener("install", function (ev) {
  ev.waitUntil(
    caches.open(CACHE)
      .then(function (c) {
        // itens independentes: uma falha pontual (rede caindo durante o
        // install) não aborta todo o service worker
        return Promise.all(SHELL.map(function (u) {
          return c.add(u).catch(function () { /* segue sem este item */ });
        }));
      })
      .then(function () { return self.skipWaiting(); })
  );
});

self.addEventListener("activate", function (ev) {
  ev.waitUntil(
    caches.keys()
      .then(function (keys) {
        return Promise.all(
          keys.filter(function (k) { return k !== CACHE; })
            .map(function (k) { return caches.delete(k); })
        );
      })
      .then(function () { return self.clients.claim(); })
  );
});

self.addEventListener("fetch", function (ev) {
  var req = ev.request;
  if (req.method !== "GET") return;
  var url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // rede primeiro; cache apenas como fallback offline
  ev.respondWith(
    fetch(req).then(function (resp) {
      if (resp.ok && SHELL.indexOf(url.pathname) !== -1) {
        var copy = resp.clone();
        caches.open(CACHE).then(function (c) { c.put(req, copy); });
      }
      return resp;
    }).catch(function () {
      return caches.match(req).then(function (hit) {
        if (hit) return hit;
        if (req.mode === "navigate") return caches.match("/index.html");
        return Response.error();
      });
    })
  );
});
