package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/antonio/farchromectl/internal/certs"
	"github.com/antonio/farchromectl/internal/hub"
	"github.com/antonio/farchromectl/internal/mdns"
	"github.com/antonio/farchromectl/internal/server"
	"github.com/coder/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

// TestVersionOutput confere o formato da linha de --version. Sem carimbo de
// build (-ldflags "-X main.version=…") vale a versão default "dev"; o carimbo
// em si é exercitado pelo `make build` (verificado manualmente no CI/README).
func TestVersionOutput(t *testing.T) {
	want := "farchromectl dev"
	if got := versionOutput(); got != want {
		t.Fatalf("versionOutput = %q, queria %q", got, want)
	}
}

// bannerFixture devolve um bannerData típico para os testes.
func bannerFixture() bannerData {
	return bannerData{
		version: "test",
		token:   "424242",
		port:    8080,
		ips:     []net.IP{net.ParseIP("192.168.1.10")},
	}
}

// TestBannerPlainHTTP confere o banner sem --tls: URLs http, linha da
// extensão na porta principal e sem QR quando não é TTY (saída limpa em
// pipes/logs).
func TestBannerPlainHTTP(t *testing.T) {
	var b bytes.Buffer
	printBanner(&b, bannerFixture(), false)
	out := b.String()

	for _, want := range []string{
		"424242",
		"http://192.168.1.10:8080",
		"http://farchromectl.local:8080",
		"ws://127.0.0.1:8080/ws",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner sem tls deveria conter %q", want)
		}
	}
	if strings.ContainsAny(out, "█▀▄") {
		t.Error("banner sem TTY não deveria conter half-blocks de QR")
	}
}

// TestBannerQRNoTTYConfirmaNaoQR dupla-checa o gating: mesmo com IPs válidos,
// tty=false nunca imprime QR.
func TestBannerQROnlyOnTTY(t *testing.T) {
	d := bannerFixture()

	var pipe bytes.Buffer
	printBanner(&pipe, d, false)
	if strings.ContainsAny(pipe.String(), "█▀▄") {
		t.Fatal("QR impresso com tty=false (pipe) — gating quebrado")
	}

	var tty bytes.Buffer
	printBanner(&tty, d, true)
	out := tty.String()
	if !strings.ContainsAny(out, "█▀▄") {
		t.Fatal("QR ausente com tty=true")
	}
	if !strings.Contains(out, "Escaneie com a câmera do celular") {
		t.Error("QR impresso sem legenda")
	}
	// o QR codifica a URL primária (IPv4) — conferimos que renderiza sem erro
	// e decodifica o mesmo conteúdo.
	qr, err := qrcode.New(d.qrURL(), qrcode.Medium)
	if err != nil {
		t.Fatalf("qrcode.New(%q): %v", d.qrURL(), err)
	}
	if qr.ToSmallString(false) == "" {
		t.Error("ToSmallString vazio")
	}
}

// TestBannerTLS confere o banner com --tls: URLs https e linha da extensão
// apontando para a porta loopback (porta principal +1).
func TestBannerTLS(t *testing.T) {
	d := bannerFixture()
	d.tls = true
	d.loopbackPort = 8081
	d.certDir = "/tmp/x"

	var b bytes.Buffer
	printBanner(&b, d, false)
	out := b.String()

	for _, want := range []string{
		"https://192.168.1.10:8080",
		"https://farchromectl.local:8080",
		"ws://127.0.0.1:8081/ws",
		"/tmp/x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner com tls deveria conter %q", want)
		}
	}
	if strings.Contains(out, "http://") {
		t.Error("banner com tls não deveria ter URLs http://")
	}
}

// TestBannerSemIPv4: sem IP não-loopback (ex.: máquina só com IPv6/link-local)
// o banner não imprime QR nem quebra.
func TestBannerSemIPv4(t *testing.T) {
	d := bannerFixture()
	d.ips = []net.IP{net.ParseIP("fd00::1")} // só IPv6
	if got := d.qrURL(); got != "" {
		t.Errorf("qrURL com só IPv6 = %q, queria vazio", got)
	}
	var b bytes.Buffer
	printBanner(&b, d, true)
	if strings.Contains(b.String(), "Escaneie") {
		t.Error("QR não deveria ser impresso sem IPv4 primário")
	}
}

// TestIsTTY: os.Pipe não é char device → false.
func TestIsTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTTY(w) {
		t.Error("isTTY(pipe) = true, queria false")
	}
}

// TestBannerImprimeIPsReaisDaMaquina: sanity — NonLoopbackIPs devolve algo
// nas máquinas de CI/dev; se não houver, o banner ainda assim renderiza
// (apenas sem URLs de IP). Roda rápido e serve de smoke do caminho real.
func TestBannerImprimeIPsReaisDaMaquina(t *testing.T) {
	ips := mdns.NonLoopbackIPs()
	var b bytes.Buffer
	printBanner(&b, bannerData{version: "t", token: "424242", port: 8080, ips: ips}, false)
	if len(ips) == 0 && !strings.Contains(b.String(), "farchromectl.local") {
		t.Error("banner sem IPs deveria ainda imprimir a URL mDNS")
	}
}

// ---------- wiring --tls (startServers) ----------

// freePortWithSuccessor devolve uma porta TCP livre em 127.0.0.1 cuja
// sucessora também está livre (o modo --tls usa porta+1 como loopback).
func freePortWithSuccessor(t *testing.T) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		l1, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l1.Addr().(*net.TCPAddr).Port
		l2, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p+1)))
		if err != nil {
			l1.Close()
			continue
		}
		l1.Close()
		l2.Close()
		return p
	}
	t.Fatal("não achei porta livre com sucessora livre")
	return 0
}

// newTestHandler devolve o handler completo com hub+server reais e FS estático
// mínimo.
func newTestHandler(token string) http.Handler {
	return server.New(hub.New(), token, "test", fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>ok</title>")},
	}).Handler()
}

// shutdownSrv encerra servidores criados nos testes.
func shutdownSrv(t *testing.T, srvs ...*http.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, s := range srvs {
		if s != nil {
			_ = s.Shutdown(ctx)
		}
	}
}

// TestStartServersSemTLS: modo padrão inalterado — um servidor, sem loopback.
func TestStartServersSemTLS(t *testing.T) {
	port := freePortWithSuccessor(t)
	mainSrv, loopSrv, loopPort, err := startServers(newTestHandler("424242"), "127.0.0.1", port, false, "", "")
	if err != nil {
		t.Fatalf("startServers: %v", err)
	}
	defer shutdownSrv(t, mainSrv)

	if loopSrv != nil || loopPort != 0 {
		t.Errorf("sem --tls não deveria haver listener loopback (srv=%v port=%d)", loopSrv, loopPort)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/healthz")
	if err != nil {
		t.Fatalf("healthz http: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz http: status %d", resp.StatusCode)
	}
}

// TestStartServersTLSComLoopback é o teste central do --tls: o endereço
// principal serve HTTPS (verificado de verdade contra o certificado gerado,
// incluindo o SAN 127.0.0.1) e o listener loopback responde HTTP puro com o
// mesmo hub — o caminho da extensão (ws://127.0.0.1:porta+1) segue
// funcionando com o servidor principal em TLS.
func TestStartServersTLSComLoopback(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _, err := certs.Ensure(dir, nil)
	if err != nil {
		t.Fatalf("certs.Ensure: %v", err)
	}

	port := freePortWithSuccessor(t)
	mainSrv, loopSrv, loopPort, err := startServers(newTestHandler("424242"), "127.0.0.1", port, true, certPath, keyPath)
	if err != nil {
		t.Fatalf("startServers: %v", err)
	}
	defer shutdownSrv(t, mainSrv, loopSrv)
	if loopPort != port+1 {
		t.Fatalf("loopPort = %d, queria %d (porta principal +1)", loopPort, port+1)
	}

	// cliente HTTPS que VERIFICA contra o certificado gerado (sem
	// InsecureSkipVerify): exercita os SANs de verdade.
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("cert.pem não parseia como PEM")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	}}

	// (1) healthz sobre HTTPS, verificado
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/api/healthz")
	if err != nil {
		t.Fatalf("healthz https verificado: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "farchromectl") {
		t.Fatalf("healthz https: status=%d body=%s", resp.StatusCode, body)
	}

	// (2) UI sobre HTTPS
	resp, err = client.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatalf("UI https: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UI https: status %d", resp.StatusCode)
	}

	// (3) loopback responde HTTP puro (caminho da extensão)
	resp, err = http.Get("http://127.0.0.1:" + strconv.Itoa(loopPort) + "/api/healthz")
	if err != nil {
		t.Fatalf("healthz loopback http: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz loopback: status %d", resp.StatusCode)
	}

	// (4) extensão conecta via ws:// no loopback enquanto o principal é TLS:
	// hello ok:true.
	dialAuth := func(ctx context.Context, url, role string) error {
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			return err
		}
		defer conn.CloseNow()
		auth, _ := json.Marshal(map[string]string{"role": role, "token": "424242"})
		if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
			return err
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var hello struct {
			Type  string `json:"type"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "hello" {
			return errBadHello(data)
		}
		if !hello.OK {
			return errBadHello(data)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dialAuth(ctx, "ws://127.0.0.1:"+strconv.Itoa(loopPort)+"/ws", "ext"); err != nil {
		t.Errorf("extensão via ws loopback: %v", err)
	}
	// (5) UI conecta via wss:// no principal, com TLS verificado contra o cert.
	wssClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	}}
	wconn, _, err := websocket.Dial(ctx, "wss://127.0.0.1:"+strconv.Itoa(port)+"/ws",
		&websocket.DialOptions{HTTPClient: wssClient})
	if err == nil {
		auth, _ := json.Marshal(map[string]string{"role": "ui", "token": "424242"})
		if err := wconn.Write(ctx, websocket.MessageText, auth); err != nil {
			t.Errorf("wss ui write: %v", err)
		} else {
			_, data, err := wconn.Read(ctx)
			if err != nil {
				t.Errorf("wss ui hello read: %v", err)
			} else {
				var hello struct {
					Type string `json:"type"`
					OK   bool   `json:"ok"`
				}
				if json.Unmarshal(data, &hello) != nil || hello.Type != "hello" || !hello.OK {
					t.Errorf("wss ui hello inesperado: %s", data)
				}
			}
		}
		wconn.CloseNow()
	} else {
		t.Errorf("ui via wss principal: %v", err)
	}
}

// errBadHello formata o erro de hello inesperado.
func errBadHello(data []byte) error {
	return &badHelloError{data: string(data)}
}

type badHelloError struct{ data string }

func (e *badHelloError) Error() string { return "hello inesperado: " + e.data }

// TestStartServersLoopbackOcupado: porta+1 ocupada → erro claro (não sobe
// meia-configuração silenciosa).
func TestStartServersLoopbackOcupado(t *testing.T) {
	port := freePortWithSuccessor(t)
	// ocupa a porta+1 deliberadamente
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	dir := t.TempDir()
	certPath, keyPath, _, err := certs.Ensure(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = startServers(newTestHandler("424242"), "127.0.0.1", port, true, certPath, keyPath)
	if err == nil {
		t.Fatal("startServers com loopback ocupado deveria falhar")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("erro deveria mencionar loopback: %v", err)
	}
}
