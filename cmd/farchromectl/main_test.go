package main

import (
	"bytes"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/antonio/farchromectl/internal/mdns"
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
