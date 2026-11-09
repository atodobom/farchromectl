package mdns

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/grandcat/zeroconf"
)

// TestAdvertiseFailsSoft verifica que um erro de registro do zeroconf é
// devolvido como erro (sem pânico) e que a função stop devolvida é segura.
// O registro mDNS real é passo manual de verificação (ver README).
func TestAdvertiseFailsSoft(t *testing.T) {
	orig := registerProxy
	registerProxy = func(instance, service, domain string, port int, host string, ips []string, text []string, ifaces []net.Interface) (*zeroconf.Server, error) {
		return nil, errors.New("sem rota multicast")
	}
	t.Cleanup(func() { registerProxy = orig })

	stop, err := Advertise(8080, "0.1.0-test")
	if err == nil {
		t.Fatal("esperava erro propagado do registro")
	}
	// stop deve ser chamável sem pânico mesmo com registro falho.
	stop()
}

// TestAdvertisePropagatesIPTxt garante que o TXT anunciado carrega apenas a
// versão — nunca o token — e que o serviço/domínio/host estão fixados.
func TestAdvertisePropagatesIPTxt(t *testing.T) {
	orig := registerProxy
	var gotInstance, gotService, gotDomain, gotHost string
	var gotPort int
	var gotTxt []string
	registerProxy = func(instance, service, domain string, port int, host string, ips []string, text []string, ifaces []net.Interface) (*zeroconf.Server, error) {
		gotInstance, gotService, gotDomain, gotHost = instance, service, domain, host
		gotPort = port
		gotTxt = text
		return &zeroconf.Server{}, nil
	}
	t.Cleanup(func() { registerProxy = orig })

	stop, err := Advertise(8081, "9.9.9")
	if err != nil {
		t.Fatalf("registro simulado deveria sucesso: %v", err)
	}
	// NOTA: não chamamos stop() aqui — o Server retornado é um zero value
	// falso e o Shutdown real do zeroconf tocaria campos não inicializados.
	// A segurança de stop() no cenário de erro é coberta por
	// TestAdvertiseFailsSoft; o caminho real de shutdown é manual.
	_ = stop

	if gotInstance != Instance || gotService != Service || gotDomain != Domain || gotHost != Hostname {
		t.Fatalf("registro com parâmetros errados: %q %q %q %q", gotInstance, gotService, gotDomain, gotHost)
	}
	if gotPort != 8081 {
		t.Fatalf("porta anunciada = %d", gotPort)
	}
	if len(gotTxt) != 1 || gotTxt[0] != "version=9.9.9" {
		t.Fatalf("TXT = %v — o token JAMAIS deve aparecer aqui", gotTxt)
	}
}

func TestNonLoopbackIPsExcludesLoopback(t *testing.T) {
	ips := NonLoopbackIPs()
	for _, ip := range ips {
		if ip.IsLoopback() {
			t.Errorf("IP loopback devolvido: %v", ip)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("IP link-local devolvido: %v", ip)
		}
	}
	// Os strings devem casar com os IPs.
	strs := NonLoopbackIPStrings()
	if len(strs) != len(ips) {
		t.Fatalf("tamanhos divergem: %d ips vs %d strings", len(ips), len(strs))
	}
	for i, s := range strs {
		if s != ips[i].String() {
			t.Errorf("string[%d]=%q != ip %v", i, s, ips[i])
		}
		if strings.Count(s, ":") > 0 && !strings.Contains(s, "]") {
			// IPv6 puro só entra se realmente veio da lista; ok.
			_ = s
		}
	}
}
