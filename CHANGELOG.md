# Changelog

Todas as mudanças notáveis deste projeto serão documentadas neste arquivo.

O formato é baseado no [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/),
e este projeto adere ao [Versionamento Semântico](https://semver.org/lang/pt-BR/)
(tags anotadas `vX.Y.Z`, versão carimbada no binário via `git describe`).

## [v0.1.0] - 2026-11-09

### Adicionado

- Servidor Go em **binário único** com a UI web embutida (`go:embed`), sem
  dependências externas em tempo de execução.
- Hub WebSocket autenticado por **token** (PIN de 6 dígitos gerado na 1ª
  execução e persistido em `~/.config/farchromectl/`).
- Anúncio **mDNS** (`farchromectl.local`), fail-soft — sem mDNS, o servidor
  continua por IP.
- **Descoberta do servidor** e credencial persistente na UI (token salvo no
  navegador, com botão "Esquecer credenciais").
- Controles na UI: **play/pause, seek ±10s, scrub, volume, mute, velocidade**
  e **seletor de aba**.
- **Extensão Chrome MV3** (background, content script, página de opções) com
  auto-reconnect do service worker.
- **README em PT-BR** com instruções de build, instalação da extensão e
  solução de problemas.
- **Testes unitários e ponta-a-ponta** (dial WebSocket real) para o hub,
  config, server e mDNS.

### Limitações conhecidas

- **Sem faixa anterior/próxima** (`next`/`prev`): os handlers de Media Session
  dos sites não podem ser disparados externamente.
- **DRM**: em streams protegidos, a página pode aceitar o comando sem que ele
  tenha efeito real (o erro é exibido na UI).
- **PWA completo exige TLS** (service worker requer contexto seguro; na LAN
  usamos HTTP puro) — trabalho futuro.
- **QR code no terminal** para facilitar a abertura no celular — futuro.
- O **PIN de 6 dígitos** pressupõe uma LAN confidencial (casa); não é adequado
  para redes compartilhadas/hostis.
