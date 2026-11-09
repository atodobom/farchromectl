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
```

Opções:

| Flag       | Padrão                 | Descrição                                                        |
|------------|------------------------|------------------------------------------------------------------|
| `--addr`   | `0.0.0.0:8080`         | endereço/porta de escuta (ex.: `--addr 0.0.0.0:9090`)            |
| `--token`  | gerado/persistido      | força um token específico (6 dígitos); não altera o arquivo salvo|
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

## Instalando a extensão no Chrome (load unpacked)

1. Abra `chrome://extensions`;
2. Ative **"Modo do desenvolvedor"** (canto superior direito);
3. Clique em **"Carregar sem compactação"** e selecione a pasta `extension/`
   deste projeto;
4. Abra as **opções da extensão** (ícone ⋮ → Opções na página de extensões):
   informe a **porta** (padrão `8080`) e o **token** impresso no terminal do
   servidor;
5. Com o servidor rodando e um vídeo/áudio tocando em alguma aba, o ícone de
   "extensão" no canto da UI deve ficar verde.

A extensão não carrega código remoto — só `background.js`, `content.js` e a
página de opções locais.

## Abrindo do celular

1. Certifique-se de que o servidor está rodando e o celular está na mesma
   rede Wi-Fi;
2. Abra uma das URLs impressas no terminal:
   - `http://<IP-do-computador>:8080` (ex.: `http://192.168.0.10:8080`), ou
   - `http://farchromectl.local:8080` (nome mDNS — ver abaixo);
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

## Limitações (v0.1)

- **Sem faixa anterior/próxima** (`next`/`prev`): os handlers de Media Session
  dos sites não podem ser disparados externamente — não fingimos ter o
  botão.
- **DRM**: em streams protegidos, seek/volume podem não ter efeito; a UI
  mostra o erro em vez de falhar silenciosamente.
- **Não é PWA instalável sem HTTPS**: service worker exige contexto seguro, e
  na LAN usamos HTTP puro — por isso não há service worker. Adicionar à tela
  de início funciona normalmente. TLS com certificado local é trabalho futuro.
- **Aba alvo**: comandos vão para a "melhor" mídia detectada (tocando >
  maior > mais longa); com várias abas tocando, escolha a aba no seletor da UI.

## Desenvolvimento

```
cmd/farchromectl/   entrypoint (flags, banner, shutdown)
internal/config/    token: geração (crypto/rand) e persistência
internal/hub/       relay WebSocket ui<->ext, status de extensão
internal/server/    HTTP: /, /api/healthz, /ws (auth por token)
internal/mdns/      anúncio _farchromectl._tcp (fail-soft, sem token no TXT)
web/                UI embutida via go:embed (HTML/CSS/JS puro, sem build)
extension/          extensão Chrome MV3 (sem código remoto)
```

Testes: `go test ./...` cobre hub (relay, broadcast, reconexão da extensão),
config (persistência do token), server (healthz/CORS, rejeição de token,
fluxo ponta-a-ponta com dial WebSocket real) e mdns (falha benigna).

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
