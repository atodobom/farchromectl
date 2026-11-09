// Command farchromectl é o servidor local que serve a UI web ao celular e
// faz o relay WebSocket com a extensão do Chrome.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	printBanner(ln, token)

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

func printBanner(ln net.Listener, token string) {
	port := ln.Addr().(*net.TCPAddr).Port
	const W = 42 // largura interna das caixas

	spaced := "f a r c h r o m e c t l"
	label := "TOKEN (use no celular e na extensão)"

	fmt.Println()
	fmt.Println("  ╭" + strings.Repeat("─", W) + "╮")
	fmt.Printf("  │%*s%s%*s│\n", 9, "", spaced, 10, "")
	fmt.Println("  ╰" + strings.Repeat("─", W) + "╯")
	fmt.Printf("  versão  : %s\n", version)
	fmt.Println()
	fmt.Println("  ╔" + strings.Repeat("═", W) + "╗")
	fmt.Printf("  ║%*s%s%*s║\n", 3, "", label, 3, "")
	fmt.Printf("  ║%*s%s%*s║\n", (W-len(token))/2, "", token, (W+1-len(token))/2, "")
	fmt.Println("  ╚" + strings.Repeat("═", W) + "╝")
	fmt.Println()
	fmt.Println("  Abra no celular (mesma rede Wi-Fi):")
	for _, ip := range mdns.NonLoopbackIPs() {
		fmt.Printf("    http://%s\n", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
	fmt.Printf("    http://%s   (nome mDNS; requer avahi)\n", net.JoinHostPort(mdns.Hostname+".local", strconv.Itoa(port)))
	fmt.Println()
	fmt.Printf("  Extensão do Chrome: ws://127.0.0.1:%d/ws (configure o token na página de opções)\n", port)
	fmt.Println()
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
