# farchromectl

Controle as mídias que estão tocando no seu Chrome **a partir do celular**, na
mesma rede Wi-Fi/LAN.

```
[celular / navegador] <--WebSocket--> [servidor Go, binário único, UI embutida] <--WebSocket--> [extensão Chrome MV3] --> <video>/<audio> nas abas
```

O servidor fica rodando no computador, serve uma UI web otimizada para toque e
faz o relay dos comandos entre o celular e a extensão do Chrome. Nada sai da
sua rede local.

---

## Requisitos

- Go 1.22+ (só para compilar; o binário final é estático e único);
- Google Chrome (ou Chromium/Edge compatível com Manifest V3);
- Celular e computador na **mesma rede** (Wi-Fi de casa, por exemplo).

## Build

```sh
make build          # gera bin/farchromectl
# ou diretamente:
go build -o bin/farchromectl ./cmd/farchromectl
```

Instalar (opcional):

```sh
make install        # copia para ~/.local/bin/farchromectl
```

Testes e checagens:

```sh
make test   # go test ./...
make vet    # go vet ./...
```

## Rodando o servidor

```sh
./bin/farchromectl
```

Na primeira execução ele gera um **PIN de 6 dígitos**, salva em
`~/.config/farchromectl/config.json` e imprime no terminal:

```
  ╔══════════════════════════════════════════╗
  ║   TOKEN (use no celular e na extensão)   ║
  ║                 843127                   ║
  ╚══════════════════════════════════════════╝

  Abra no celular (mesma rede Wi-Fi):
    http://192.168.0.10:8080
    http://farchromectl.local:8080   (nome mDNS; requer avahi)

  Escaneie com a câmera do celular (abre a UI):
    ▄▄█▄█▄ ▀█ █▀▀▀▀▀█  …  ← QR da URL principal (IPv4)

  Extensão do Chrome: ws://127.0.0.1:8080/ws (configure o token na página de opções)
```

O **QR** aponta para a URL principal (primeiro IPv4 não-loopback, na porta
configurada) e só é impresso quando o stdout é um terminal — em
pipe/redirecionamento para arquivo a saída fica limpa, sem blocos Unicode.

Opções:

| Flag       | Padrão                 | Descrição                                                        |
|------------|------------------------|------------------------------------------------------------------|
| `--addr`   | `0.0.0.0:8080`         | endereço/porta de escuta (ex.: `--addr 0.0.0.0:9090`)            |
| `--token`  | gerado/persistido      | força um token específico (6 dígitos); não altera o arquivo salvo|
| `--tls`    | desligado              | serve HTTPS com certificado local self-signed — habilita PWA instalável (ver abaixo) |
| `--open`   | desligado              | abre a UI no navegador local após iniciar (se houver sessão X11/Wayland) |
| `--version`| —                      | imprime a versão do binário e sai                                |

### Token e porta

- O token é gerado **na primeira execução** e reaproveitado nas seguintes
  (fica em `~/.config/farchromectl/config.json`, permissão `0600`).
- Para trocar o token: apague o arquivo de configuração (um novo PIN é
  gerado) ou use `--token 123456`. Lembre-se de atualizar o token no celular
  ("Esquecer credenciais" nos ajustes da UI) e na extensão.
- Nunca compartilhe o token fora da sua rede: quem o tiver na LAN controla as
  mídias do seu Chrome.

## TLS e PWA instalável (`--tls`)

Por padrão o servidor fala HTTP puro: funciona direto na LAN, mas sem
service worker — e portanto sem instalação como app/PWA no celular. Com
`--tls`, o mesmo endereço/porta (`--addr`) passa a servir **HTTPS**:

```sh
./bin/farchromectl --tls
```

O que muda:

- **Certificado local self-signed**, gerado automaticamente na primeira
  execução com `--tls`: ECDSA P-256, validade de 825 dias, SANs
  `localhost`, `farchromectl.local`, `127.0.0.1` e todos os IPs locais
  não-loopback. Persistido em `~/.config/farchromectl/cert.pem` +
  `key.pem` (permissão `0600`) e **reutilizado** nos restarts — o celular
  não precisa "confiar" num certificado novo a cada vez. Se o IP da
  máquina mudar (DHCP), o certificado é regenerado para incluir o IP novo;
  reserve o IP no roteador (dica já na seção DHCP) para evitar isso.
- **PWA instalável**: a UI registra um service worker (cache do app shell,
  estratégia rede-primeiro) quando a página é servida em contexto seguro.
  Em HTTP puro nada muda — não há nem tentativa de registro. A UI também
  passa a derivar `wss://` da origem `https://` automaticamente.
- **A extensão continua funcionando sem alteração**: ela conecta via
  `ws://127.0.0.1:<porta>/ws` e não fala TLS. Com `--tls`, o servidor
  sobe adicionalmente um segundo listener **HTTP puro, restrito a
  127.0.0.1, na porta principal +1** (ex.: porta 8080 → loopback 8081),
  compartilhando o mesmo hub. Nas opções da extensão, informe a porta
  loopback (porta principal +1). O banner do terminal imprime a porta
  exata. Esse tráfego nunca sai da máquina, então não há ganho de
  segurança em convertê-lo para `wss://` — só um passo extra de
  configuração (importar certificado no Chrome).

### Confiar no certificado no celular (visão geral)

O certificado é self-signed: o celular avisará na primeira visita. Para o
PWA instalar e o service worker funcionar, o certificado precisa ser
confiado no sistema:

- **Android**: Settings → Security → Encryption & credentials → Install a
  certificate → CA certificate → selecione o `cert.pem` transferido do
  computador (o arquivo tem `IsCA`/`KeyUsageCertSign` justamente para
  ser instalável como CA de usuário). Ajuste fino por fabricante.
- **iOS**: envie o `cert.pem` para o celular (AirDrop/e-mail/arquivo),
  toque nele → instalado como perfil em Settings → Profile Downloaded →
  depois Settings → General → About → Certificate Trust Settings →
  ative a confiança total para o certificado.

O passo a passo assistido (com o celular real do usuário, incluindo o QR e
a instalação do PWA) faz parte da validação manual da versão 0.2.

## Instalando a extensão no Chrome (load unpacked)

1. Abra `chrome://extensions`;
2. Ative **"Modo do desenvolvedor"** (canto superior direito);
3. Clique em **"Carregar sem compactação"** e selecione a pasta `extension/`
   deste projeto;
4. Abra as **opções da extensão** (ícone ⋮ → Opções na página de extensões):
   informe a **porta** e o **token** impressos no terminal do servidor —
   porta `8080` no modo padrão; com `--tls`, a **porta loopback** (porta
   principal +1, ex.: `8081` — ver seção TLS);
5. Com o servidor rodando e um vídeo/áudio tocando em alguma aba, o ícone de
   "extensão" no canto da UI deve ficar verde.

A extensão não carrega código remoto — só `background.js`, `content.js` e a
página de opções locais.

## Abrindo do celular

1. Certifique-se de que o servidor está rodando e o celular está na mesma
   rede Wi-Fi;
2. Abra uma das URLs impressas no terminal — ou **escaneie o QR** do banner
   com a câmera, que já abre a URL principal:
   - `http://<IP-do-computador>:8080` (ex.: `http://192.168.0.10:8080`), ou
   - `http://farchromectl.local:8080` (nome mDNS — ver abaixo);
   - com `--tls` as URLs (e o QR) usam `https://` — o celular pedirá
     confiança no certificado na primeira vez (ver seção TLS);
3. Digite o token quando pedido. Ele fica salvo no navegador e não será
   pedido de novo (enquanto não mudar).

### Firewall

Se o celular não conseguir abrir a URL, libere a porta no firewall do
computador:

```sh
# Debian/Ubuntu (ufw):
sudo ufw allow 8080/tcp

# Fedora/RHEL (firewalld):
sudo firewall-cmd --add-port=8080/tcp --permanent && sudo firewall-cmd --reload
```

### Nome mDNS (`farchromectl.local`)

Para o nome `farchromectl.local` funcionar, o computador precisa responder
mDNS — no Linux, instale/habilite o `avahi-daemon`:

```sh
sudo apt install avahi-daemon     # Debian/Ubuntu
sudo systemctl enable --now avahi-daemon
```

Se o mDNS falhar, o servidor apenas imprime um aviso e continua funcionando
por IP.

### IP do computador muda toda hora? (DHCP)

A UI tenta se reconectar sozinha: primeiro o último endereço que funcionou,
depois `http://farchromectl.local:<porta>` e, por fim, cai no formulário
manual. Ainda assim, a recomendação é **reservar o IP do computador no
roteador** (vincular ao MAC address, geralmente em "DHCP → Address
Reservation"). Com IP fixo, o celular conecta sempre no primeiro endereço.

### iOS / Safari

No iOS, sites adicionados à tela de início ("Adicionar à Tela de Início")
mantêm o `localStorage` separado e estável — recomendado para o token não ser
pedido a cada reinstalação do Safari. Em navegadores com "bloqueio de
rastreio" agressivo, se o token sumir, use o botão "Esquecer credenciais" e
entre de novo.

## Solução de problemas

| Sintoma | Causa provável | O que fazer |
|---|---|---|
| Ícone "extensão" fica vermelho na UI | extensão não conectou ao servidor | confira **porta e token** nas opções da extensão; o servidor precisa estar rodando |
| UI não encontra o servidor | rede/firewall/IP mudou | mesma rede Wi-Fi? liberar porta no firewall (acima); anotar o IP novo do terminal |
| "Nenhuma mídia detectada" | não há `<video>`/`<audio>` na aba | dê play em algum vídeo/áudio primeiro; páginas internas do Chrome (`chrome://…`) não têm content script |
| Comando não tem efeito | DRM/stream protegido | alguns sites protegidos ignoram seek/volume externos — o erro é exibido na UI; não há contorno genérico |
| `farchromectl.local` não abre | mDNS indisponível | instale/habilite `avahi-daemon` (acima) ou use o IP |
| Extensão "adormece" após muito tempo sem mídia | service worker MV3 é suspenso pelo Chrome | normal: ao dar play em qualquer mídia (ou abrir as opções), ela reconecta sozinha em segundos |
| Celular mostra aviso de segurança ao abrir com `--tls` | certificado self-signed ainda não confiando no celular | siga os passos de confiança (seção TLS); enquanto não confiar, dá para "prosseguir" e usar a UI, só sem PWA/service worker |
| Extensão não conecta com `--tls` ligado | porta nas opções da extensão errada | use a porta **loopback** (porta principal +1, impressa no banner), não a porta principal |
| `--tls` não sobe: "listener loopback … indisponível" | porta principal +1 ocupada por outro processo | use `--addr` com outra porta (o listener da extensão é sempre porta+1) |

## Limitações (v0.1)

- **Sem faixa anterior/próxima** (`next`/`prev`): os handlers de Media Session
  dos sites não podem ser disparados externamente — não fingimos ter o
  botão.
- **DRM**: em streams protegidos, seek/volume podem não ter efeito; a UI
  mostra o erro em vez de falhar silenciosamente.
- **PWA exige `--tls`**: em HTTP puro pela LAN não há service worker (exige
  contexto seguro) — "Adicionar à tela de início" funciona, mas sem cache
  offline. Com `--tls` a UI vira PWA instalável (com o certificado confiado
  no celular; ver seção TLS). O ícone em SVG pode não ser aceito por todos
  os navegadores para o prompt de instalação — se o prompt não aparecer,
  o "Adicionar à tela de início" segue disponível.
- **Aba alvo**: comandos vão para a "melhor" mídia detectada (tocando >
  maior > mais longa); com várias abas tocando, escolha a aba no seletor da UI.

## Desenvolvimento

```
cmd/farchromectl/   entrypoint (flags, banner+QR, TLS/loopback, shutdown)
internal/config/    token: geração (crypto/rand) e persistência
internal/certs/     --tls: cert self-signed ECDSA (gera/persiste/reutiliza)
internal/hub/       relay WebSocket ui<->ext, status de extensão
internal/server/    HTTP: /, /api/healthz, /ws (auth por token)
internal/mdns/      anúncio _farchromectl._tcp (fail-soft, sem token no TXT)
web/                UI embutida via go:embed (HTML/CSS/JS puro + sw.js, sem build)
extension/          extensão Chrome MV3 (sem código remoto)
e2e/                harness E2E com Chrome real (modo plano e -tls)
```

Testes: `go test ./...` cobre hub (relay, broadcast, reconexão da extensão),
config (persistência do token), certs (geração/SANs/0600/reuso entre
execuções), server (healthz/CORS, rejeição de token, lockout, fluxo
ponta-a-ponta com dial WebSocket real), cmd (banner/QR/gating TTY, par de
listeners TLS+loopback) e mdns (falha benigna).

### Teste E2E com Chrome real

```sh
make e2e        # modo padrão (HTTP puro)
make e2e-tls    # modo --tls (HTTPS/wss/service worker/extensão via loopback)
```

Sobe o servidor em porta de teste com token conhecido, lança um **Chrome
dedicado** (perfil temporário, headless, porta de debug própria — não toca
no Chrome/perfil do usuário), instala a **extensão de produção** via CDP
`Extensions.loadUnpacked` (necessário porque o Chrome 137+ branded ignora
`--load-extension`), abre uma página com mídia local (vídeo via `ffmpeg`,
com fallback para `<audio>` com WAV sintetizado em JS) e valida a cadeia
completa: extensão conecta (`extStatus`), `state` reporta a mídia tocando e
`pause`/`play`/`seekTo`/`setVolume` refletem no elemento. Requer
google-chrome/chromium instalado; o play é disparado com clique real via
CDP (`Input.dispatchMouseEvent`), respeitando a política de autoplay.

No modo `-tls` (`make e2e-tls`), o servidor roda com `--tls` (certificado
gerado em diretório temporário via `XDG_CONFIG_HOME` — o `~/.config` do
usuário não é tocado), o lado Go verifica o HTTPS/wss **de verdade** contra
esse certificado (sem desativar verificação), o Chrome recebe
`--ignore-certificate-errors` e o harness valida adicionalmente: UI servida
por `https:` em contexto seguro, a UI do próprio navegador autenticando por
`wss` (indicadores servidor+extensão acesos), o service worker registrado e
pronto (`navigator.serviceWorker.ready`) e a extensão de produção
conectando pelo listener HTTP loopback (porta principal +1) com a cadeia
completa de comandos passando igual ao modo plano.

## Versionamento

- Este projeto segue [SemVer](https://semver.org/lang/pt-BR/):
  `MAJOR.MINOR.PATCH` — mudanças incompatíveis, funcionalidades novas e
  correções, nessa ordem.
- Cada release é marcado com uma **tag anotada**: `git tag -a vX.Y.Z -m "..."`.
- A versão é **carimbada no binário** no `make build`, a partir do
  `git describe --tags --always --dirty` (via
  `-ldflags "-X main.version=…"`).
- Para conferir a versão de um binário: `./bin/farchromectl --version`
  (também exposta no `/api/healthz` e no anúncio mDNS).
- Histórico de mudanças por versão: [CHANGELOG.md](CHANGELOG.md).
