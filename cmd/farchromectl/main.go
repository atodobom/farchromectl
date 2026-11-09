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

	h := hub.New()
	srv := &http.Server{
		Handler: server.New(h, token, version, web.Files()).Handler(),
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		log.Fatalf("não foi possível escutar em %s: %v", *addr, err)
	}

	// mDNS é opcional: falha aqui não derruba o servidor.
	if stop, err := mdns.Advertise(ln.Addr().(*net.TCPAddr).Port, version); err != nil {
		log.Printf("AVISO: mDNS indisponível (%v); use o IP direto. Dica: instale/habilite o avahi-daemon.", err)
	} else {
		defer stop()
	}

	printBanner(os.Stdout, bannerData{
		version: version,
		token:   token,
		port:    port,
		ips:     mdns.NonLoopbackIPs(),
	}, isTTY(os.Stdout))

	if *open && graphicalSession() {
		go openBrowser(ln)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("servidor: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("encerrando…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
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

func openBrowser(ln net.Listener) {
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://localhost:%d", port)
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		log.Printf("AVISO: não foi possível abrir %s: %v", url, err)
		return
	}
	_ = cmd.Wait() // evita zumbi
}
