# Goal — farchromectl v0.2: validação ponta-a-ponta e experiência mobile

## Objetivo e estado final esperado
farchromectl validado de ponta a ponta — servidor ↔ extensão ↔ mídia real em um Chrome de verdade (cadeia automatizada), e UI funcionando no celular do usuário via mDNS/IP com credencial persistente — mais hardening de autenticação e as melhorias de UX pendentes (QR no terminal; TLS opcional para PWA instalável). Estado final: tag `v0.2.0` no main, CHANGELOG atualizado, evidências de verificação anexadas ao relatório de closeout.

## Contexto
v0.1.0 implementado e taggeado (histórico: merge d4a65b4 + versionamento 0d9f7e9). Não verificado até agora: extensão em Chrome real, mDNS com resolvedor real, UI em navegador mobile. Future work declarado no CHANGELOG: QR no terminal, TLS/PWA, rate-limit no handshake WS.

## Fontes de verdade
- README.md e CHANGELOG.md do repo (decisões e limitações v0.1)
- Catálogo de comandos/protocolo: internal/hub, web/app.js, extension/{background,content}.js
- Guia do harness: ~/.codex/guides/goal-authoring.md (formato/estados finais)

## Premissas
- Chrome/Chromium instalado na máquina (verificar; se ausente, Fase 1 = Blocked)
- O Chrome diário do usuário roda com CDP na 9222 — NÃO tocar nele; usar instância dedicada em porta/perfil separados
- Celular do usuário disponível apenas na Fase 4 (validação assistida)

## Escopo (fases)
- **Fase 1 — E2E automatizado local**: lançar Chrome dedicado (--user-data-dir temporário, --load-extension, --remote-debugging-port dedicado; headless=new ou Xvfb se necessário), página de teste com mídia gerada localmente (vídeo via ffmpeg se disponível, senão <audio> com WAV data-URI gerado em JS), binário farchromectl em porta de teste com token conhecido; validar pela CDP a cadeia completa: extensão conecta (extStatus), state reporta mídia tocando, comandos pause/seek/volume refletem no elemento. Corrigir bugs encontrados (bounded).
- **Fase 2 — Hardening**: rate-limit/tentativas no handshake WS do token (ex.: lockout temporário após N falhas), testes.
- **Fase 3 — UX**: QR no terminal no banner de startup (dependência github.com/skip2/go-qrcode permitida como exceção deliberada); flag --tls com certificado self-signed auto-gerado para PWA instalável completo (service worker + manifest; desligado por padrão).
- **Fase 4 — Validação assistida pelo usuário**: checklist claro (instalar extensão no Chrome diário, abrir URL do celular, testar mDNS/controles); tag v0.2.0 + CHANGELOG após confirmação.

## Fora de escopo
next/prev track (limitação de plataforma), workarounds para DRM, acesso além da LAN (tunnels/exposição pública), multiusuário.

## Regras obrigatórias
- Commits convencionais; testes verdes (go vet/gofmt/go test ./...) antes de cada merge em main; branches work/* por fase; SEM remotes/push; não quebrar o protocolo v0.1 (UI/extensão existentes devem continuar funcionando).

## Restrições e invariantes
- Não tocar no Chrome diário do usuário (porta 9222/perfil pessoal); instâncias de teste dedicadas e limpas ao final
- Token nunca vai em TXT mDNS; arquivo de config permanece 0600
- Dependências externas novas: apenas skip2/go-qrcode (Fase 3)

## Entregáveis
Harness de teste E2E reutilizável (script/Makefile target `make e2e`), hardening com testes, QR, --tls + SW/manifest, README/CHANGELOG atualizados, tag v0.2.0, evidências.

## Critérios de aceitação
1. `make e2e` roda a cadeia real Chrome+extensão+servidor e passa (ou, se ambiente impedir, evidência do bloqueio exato)
2. go test ./... verde incluindo novos testes de rate-limit
3. Banner de startup mostra QR renders sem erro; --tls serve HTTPS com SW registrável (validado via CDP)
4. Usuário confirma controle pelo celular (único critério gated pelo usuário; sem ele → estado Partially completed)
5. Tag v0.2.0 no tip do main, git describe limpo

## Verificação e evidências
Comandos e saídas no relatório de cada fase; para Fase 1: transcript CDP/estados observados; Fase 4: confirmação textual do usuário.

## Política de execução e iteração
Fases sequenciais, cada uma despachada pelo prompter ao orchestrator; dentro de cada fase, iterar livremente com checkpoints git; em falha persistente, resetar ao checkpoint e reportar; escalonar ambiguidade material ao prompter antes de seguir.

## Alternativas e problemas pré-existentes
Se headless=new não carregar extensão: tentar headed+Xvfb; se ambos falharem, Fase 1 = Blocked com evidência (não improvisar validação simulada). mDNS real depende de avahi/resolved — verificar com `avahi-browse`/`resolvectl query farchromectl.local` se disponível; ausência não bloqueia.

## Condições de bloqueio e interrupção
Chrome indisponível; extensão impossível de carregar em qualquer modo; API MV3 incompatível de forma não contornável; qualquer risco de tocar o Chrome/perfil do usuário. Nesses casos: parar, registrar ponto exato, estado = Blocked.

## Closeout
Relatório final com: fases concluídas, evidências, estado final (Completed/Partially completed/Blocked/Safely interrupted), itens pendentes e ação de retomada. CHANGELOG v0.2.0 + tag após Fase 4.
