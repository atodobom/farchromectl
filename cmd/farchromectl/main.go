// Command farchromectl é o servidor local que serve a UI web ao celular e
// faz o relay WebSocket com a extensão do Chrome.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/antonio/farchromectl/internal/certs"
	"github.com/antonio/farchromectl/internal/config"
	"github.com/antonio/farchromectl/internal/hub"
	"github.com/antonio/farchromectl/internal/mdns"
	"github.com/antonio/farchromectl/internal/server"
	"github.com/antonio/farchromectl/web"
)

// version é carimbada no build via
// -ldflags "-X main.version=$(git describe --tags --always --dirty)";
// sem carimbo, vale "dev".
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("farchromectl: ")

	addr := flag.String("addr", "0.0.0.0:8080", "endereço de escuta (host:porta)")
	tokenFlag := flag.String("token", "", "token de autenticação (padrão: PIN de 6 dígitos gerado na 1ª execução e salvo em ~/.config/farchromectl/)")
	tlsFlag := flag.Bool("tls", false, "serve HTTPS com certificado local self-signed (habilita PWA instalável no celular); a extensão continua em HTTP puro só-loopback na porta principal +1")
	open := flag.Bool("open", false, "abre a UI no navegador local após iniciar (requer sessão gráfica)")
	showVersion := flag.Bool("version", false, "imprime a versão do binário e sai")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionOutput())
		os.Exit(0)
	}

	token, err := resolveToken(*tokenFlag)
	if err != nil {
		log.Fatalf("falha ao resolver token: %v", err)
	}

	host, portStr, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("--addr inválido %q: %v", *addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		log.Fatalf("porta inválida em --addr: %q", portStr)
	}
	if host == "" {
		host = "0.0.0.0"
	}

	// --tls: garante certificado (gera na 1ª vez, reutiliza depois) e define
	// a porta loopback da extensão (porta principal +1, determinística).
	useTLS := *tlsFlag
	loopbackPort, certDir, certPath, keyPath := 0, "", "", ""
	if useTLS {
		if port >= 65535 {
			log.Fatalf("--tls: porta %d não deixa sucessora para o listener loopback da extensão (porta+1) — use --addr com porta menor", port)
		}
		loopbackPort = port + 1
		var err error
		if certDir, err = certs.DefaultDir(); err != nil {
			log.Fatalf("--tls: %v", err)
		}
		var reused bool
		if certPath, keyPath, reused, err = certs.Ensure(certDir, certs.MachineIPs()); err != nil {
			log.Fatalf("--tls: certificado: %v", err)
		}
		if !reused {
			log.Printf("--tls: certificado gerado em %s (SANs: localhost, farchromectl.local, IPs locais)", certDir)
		}
	}

	h := hub.New()
	handler := server.New(h, token, version, web.Files()).Handler()

	mainSrv, loopSrv, _, err := startServers(handler, host, port, useTLS, certPath, keyPath)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// mDNS é opcional: falha aqui não derruba o servidor.
	if stop, err := mdns.Advertise(port, version); err != nil {
		log.Printf("AVISO: mDNS indisponível (%v); use o IP direto. Dica: instale/habilite o avahi-daemon.", err)
	} else {
		defer stop()
	}

	printBanner(os.Stdout, bannerData{
		version:      version,
		token:        token,
		port:         port,
		tls:          useTLS,
		loopbackPort: loopbackPort,
		certDir:      certDir,
		ips:          mdns.NonLoopbackIPs(),
	}, isTTY(os.Stdout))

	if *open && graphicalSession() {
		go openBrowser(port, useTLS)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("encerrando…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mainSrv.Shutdown(ctx)
	if loopSrv != nil {
		_ = loopSrv.Shutdown(ctx)
	}
}

// startServers cria e sobe o(s) servidor(es) HTTP com o handler dado:
//   - sem tls: um único listener (host:port) em HTTP puro — comportamento
//     idêntico ao de antes da flag existir;
//   - com tls: o listener principal (host:port) serve HTTPS (cert/key) e um
//     SEGUNDO listener, restrito a 127.0.0.1:porta+1, serve HTTP puro com o
//     mesmo handler.
//
// O listener loopback existe porque a extensão conecta via
// ws://127.0.0.1:<porta>/ws e não fala TLS — com a porta principal em HTTPS,
// ela quebraria. Alternativa considerada: mudar a extensão para wss:// e
// confiar no certificado dentro do Chrome; rejeitada porque exige um passo
// extra de configuração (importar/aceitar certificado no Chrome) sem ganho de
// segurança real: o tráfego loopback nunca sai da máquina. O caminho da
// extensão fica em HTTP puro, porém acessível somente via loopback; a porta é
// determinística (porta principal +1) para ser documentada e configurada uma
// única vez nas opções da extensão.
func startServers(handler http.Handler, host string, port int, useTLS bool, certPath, keyPath string) (mainSrv, loopSrv *http.Server, loopPort int, err error) {
	serve := func(srv *http.Server, ln net.Listener) {
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("servidor: %v", err)
			}
		}()
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("não foi possível escutar em %s:%d: %w", host, port, err)
	}
	mainSrv = &http.Server{Handler: handler}

	if !useTLS {
		serve(mainSrv, ln)
		return mainSrv, nil, 0, nil
	}

	loopLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
	if err != nil {
		_ = ln.Close()
		return nil, nil, 0, fmt.Errorf("--tls: listener loopback 127.0.0.1:%d (extensão) indisponível: %w — use --addr com outra porta", port+1, err)
	}
	loopSrv = &http.Server{Handler: handler}
	serve(loopSrv, loopLn)
	go func() {
		if err := mainSrv.ServeTLS(ln, certPath, keyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("servidor: %v", err)
		}
	}()
	return mainSrv, loopSrv, port + 1, nil
}

// versionOutput é a linha impressa por --version (separada para ser testável).
func versionOutput() string {
	return "farchromectl " + version
}

// resolveToken usa o token da flag quando informado; caso contrário carrega
// (ou gera na 1ª execução) o PIN persistido.
func resolveToken(flagToken string) (string, error) {
	if flagToken != "" {
		if !config.ValidToken(flagToken) {
			return "", fmt.Errorf("token via --token deve ter 6 dígitos")
		}
		return flagToken, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return cfg.Token, nil
}

// bannerData concentra o que o banner de startup imprime. Separado da coleta
// (listener, mDNS, flags) para ser testável sem subir servidor.
type bannerData struct {
	version      string
	token        string
	port         int
	tls          bool     // --tls: URLs https + linha do listener loopback
	loopbackPort int      // porta HTTP loopback da extensão quando tls
	certDir      string   // diretório do certificado (informativo, tls)
	ips          []net.IP // IPs não-loopback para as URLs
}

// scheme é o esquema das URLs impressas.
func (d bannerData) scheme() string {
	if d.tls {
		return "https"
	}
	return "http"
}

// qrURL devolve a URL primária codificada no QR: o primeiro IPv4 não-loopback
// (o mais útil para o celular). Vazio quando não há IPv4.
func (d bannerData) qrURL() string {
	for _, ip := range d.ips {
		if ip.To4() != nil {
			return d.scheme() + "://" + net.JoinHostPort(ip.String(), strconv.Itoa(d.port))
		}
	}
	return ""
}

// printBanner imprime o banner de startup em w. O QR só é impresso quando
// tty=true: em saída pipe/arquivo os half-blocks são lixo — o log deve ficar
// limpo. A renderização usa ToSmallString(false) (módulos claros em glyph de
// primeiro plano), que dá a polaridade correta (escuro-sobre-claro) em
// terminais com fundo escuro, o caso comum de quem roda servidor no terminal.
func printBanner(w io.Writer, d bannerData, tty bool) {
	const W = 42 // largura interna das caixas

	spaced := "f a r c h r o m e c t l"
	label := "TOKEN (use no celular e na extensão)"

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ╭"+strings.Repeat("─", W)+"╮")
	fmt.Fprintf(w, "  │%*s%s%*s│\n", 9, "", spaced, 10, "")
	fmt.Fprintln(w, "  ╰"+strings.Repeat("─", W)+"╯")
	fmt.Fprintf(w, "  versão  : %s\n", d.version)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ╔"+strings.Repeat("═", W)+"╗")
	fmt.Fprintf(w, "  ║%*s%s%*s║\n", 3, "", label, 3, "")
	fmt.Fprintf(w, "  ║%*s%s%*s║\n", (W-len(d.token))/2, "", d.token, (W+1-len(d.token))/2, "")
	fmt.Fprintln(w, "  ╚"+strings.Repeat("═", W)+"╝")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Abra no celular (mesma rede Wi-Fi):")
	for _, ip := range d.ips {
		fmt.Fprintf(w, "    %s://%s\n", d.scheme(), net.JoinHostPort(ip.String(), strconv.Itoa(d.port)))
	}
	fmt.Fprintf(w, "    %s://%s   (nome mDNS; requer avahi)\n", d.scheme(), net.JoinHostPort(mdns.Hostname+".local", strconv.Itoa(d.port)))
	fmt.Fprintln(w)
	if tty {
		if url := d.qrURL(); url != "" {
			qr, err := qrcode.New(url, qrcode.Medium)
			if err != nil {
				fmt.Fprintf(w, "  AVISO: QR indisponível (%v); use as URLs acima.\n", err)
			} else {
				fmt.Fprintln(w, "  Escaneie com a câmera do celular (abre a UI):")
				for _, line := range strings.Split(strings.TrimSuffix(qr.ToSmallString(false), "\n"), "\n") {
					fmt.Fprintln(w, "    "+line)
				}
				fmt.Fprintln(w)
			}
		}
	}
	if d.tls {
		fmt.Fprintf(w, "  Extensão do Chrome: ws://127.0.0.1:%d/ws  (HTTP só-loopback, porta principal +1 — use esta porta nas opções da extensão)\n", d.loopbackPort)
		fmt.Fprintf(w, "  TLS: certificado local self-signed em %s (o celular pedirá confiança na 1ª vez — ver README)\n", d.certDir)
	} else {
		fmt.Fprintf(w, "  Extensão do Chrome: ws://127.0.0.1:%d/ws (configure o token na página de opções)\n", d.port)
	}
	fmt.Fprintln(w)
}

// isTTY informa se f é um terminal (char device), via Stat — sem dependências
// extras. Usado para decidir se o QR é impresso.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func graphicalSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func openBrowser(port int, useTLS bool) {
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://localhost:%d", scheme, port)
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		log.Printf("AVISO: não foi possível abrir %s: %v", url, err)
		return
	}
	_ = cmd.Wait() // evita zumbi
}
