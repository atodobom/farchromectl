# Changelog

Todas as mudanças notáveis deste projeto serão documentadas neste arquivo.

O formato é baseado no [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/),
e este projeto adere ao [Versionamento Semântico](https://semver.org/lang/pt-BR/)
(tags anotadas `vX.Y.Z`, versão carimbada no binário via `git describe`).

## [v0.2.0] - 2026-11-09

### Adicionado

- **Harness E2E com Chrome real** (`make e2e`): servidor + extensão de
  produção + mídia local, validado via CDP (`Extensions.loadUnpacked`,
  clique real via `Input.dispatchMouseEvent` para respeitar a política de
  autoplay) — extensão conecta (`extStatus`), `state` reporta a mídia
  tocando e `pause`/`play`/`seekTo`/`setVolume` refletem no elemento.
  Chrome dedicado (perfil temporário, porta de debug própria) — o Chrome
  diário do usuário não é tocado.
- **Lockout por IP no handshake WS**: 5 tokens errados consecutivos do
  mesmo IP bloqueiam novas tentativas por 60s (lockout estrito: durante a
  janela o token nem é lido, e tentativas recusadas não renovam a janela);
  sequência decaiu após 10min; handshake bem-sucedido zera o estado.
  Comparação do token em tempo constante.
- **QR code no banner de startup** (URL principal, primeiro IPv4;
  impresso só quando o stdout é TTY — saída em pipe/arquivo fica limpa).
- **Flag `--tls`**: certificado self-signed ECDSA P-256 persistido
  (`~/.config/farchromectl/cert.pem`+`key.pem`, 0600, validade 825 dias,
  SANs localhost/farchromectl.local/IPs locais) + listener **HTTP puro
  restrito ao loopback na porta principal +1** para a extensão — a UI
  passa a servir HTTPS/wss no endereço principal.
- **PWA instalável sob TLS**: service worker + registro em contexto
  seguro (`make e2e-tls` valida de verdade: HTTPS verificado contra o
  cert de teste, UI do navegador autenticando por wss, SW pronto via
  `navigator.serviceWorker.ready` e extensão conectando pelo listener
  loopback).

### Corrigido

- Flake preexistente no teste ponta-a-ponta WS (`TestWSEndToEnd`): a
  entrega entre canais é eventual, não ordenada — o teste agora espera
  eventualidade em vez de ordem.

### Notas de validação

- Validado pelo usuário em um **celular real**, com `--tls`: extensão
  conectada e controles (play/pause/seek/volume) funcionando a partir do
  telefone ("Funcionou perfeitamente").

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
