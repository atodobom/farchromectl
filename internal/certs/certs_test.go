package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// parseCert lê e parseia o certificado de path.
func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ler %s: %v", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s: sem bloco PEM", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsear cert de %s: %v", path, err)
	}
	return cert
}

// TestEnsureGeraEPersiste confere geração na 1ª chamada: arquivos 0600,
// ECDSA P-256, SANs exigidos (localhost, farchromectl.local, 127.0.0.1 e os
// IPs pedidos) e validade de ~825 dias.
func TestEnsureGeraEPersiste(t *testing.T) {
	dir := t.TempDir()
	ips := []net.IP{net.ParseIP("198.51.100.20")}

	certPath, keyPath, reused, err := Ensure(dir, ips)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if reused {
		t.Error("1ª chamada deveria gerar (reused=false)")
	}

	for _, p := range []string{certPath, keyPath} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s: modo %v, queria 0600", p, st.Mode().Perm())
		}
	}

	cert := parseCert(t, certPath)
	if cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("algoritmo = %v, queria ECDSA", cert.PublicKeyAlgorithm)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("chave pública não é ECDSA P-256: %T", cert.PublicKey)
	}
	if !cert.IsCA || cert.CheckSignatureFrom(cert) != nil {
		// self-signed: assinado pela própria chave
		t.Errorf("certificado não é self-signed (CA/assinatura própria falhou)")
	}

	hasDNS := map[string]bool{}
	for _, d := range cert.DNSNames {
		hasDNS[d] = true
	}
	for _, want := range []string{"localhost", "farchromectl.local"} {
		if !hasDNS[want] {
			t.Errorf("SAN DNS faltando: %q (tem %v)", want, cert.DNSNames)
		}
	}
	hasIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		hasIP[ip.String()] = true
	}
	for _, want := range []string{"127.0.0.1", "198.51.100.20"} {
		if !hasIP[want] {
			t.Errorf("SAN IP faltando: %q (tem %v)", want, cert.IPAddresses)
		}
	}

	// validade: ~825 dias (tolerância de 1h de NotBefore)
	want := 825 * 24 * time.Hour
	got := cert.NotAfter.Sub(cert.NotBefore.Add(time.Hour))
	if got < want-time.Hour || got > want+time.Hour {
		t.Errorf("validade = %v, queria ~%v", got, want)
	}
}

// TestEnsureReutiliza: a 2ª chamada com os mesmos IPs mantém exatamente o
// mesmo par (bytes idênticos) — o celular não precisa re-confiar a cada
// restart.
func TestEnsureReutiliza(t *testing.T) {
	dir := t.TempDir()
	ips := []net.IP{net.ParseIP("198.51.100.20")}

	certPath, keyPath, reused, err := Ensure(dir, ips)
	if err != nil || reused {
		t.Fatalf("1º Ensure: reused=%v err=%v", reused, err)
	}
	cert1, _ := os.ReadFile(certPath)
	key1, _ := os.ReadFile(keyPath)

	certPath2, keyPath2, reused, err := Ensure(dir, ips)
	if err != nil {
		t.Fatalf("2º Ensure: %v", err)
	}
	if !reused {
		t.Error("2º Ensure deveria reutilizar (reused=true)")
	}
	if certPath2 != certPath || keyPath2 != keyPath {
		t.Errorf("caminhos mudaram: %s/%s vs %s/%s", certPath2, keyPath2, certPath, keyPath)
	}
	cert2, _ := os.ReadFile(certPath2)
	key2, _ := os.ReadFile(keyPath2)
	if string(cert1) != string(cert2) || string(key1) != string(key2) {
		t.Error("par reutilizado deveria ter bytes idênticos ao original")
	}
}

// TestEnsureRegeneraQuandoIPMudou: IP novo fora dos SANs → regenera com o IP.
func TestEnsureRegeneraQuandoIPMudou(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := Ensure(dir, []net.IP{net.ParseIP("198.51.100.20")}); err != nil {
		t.Fatalf("1º Ensure: %v", err)
	}
	certPath, _, reused, err := Ensure(dir, []net.IP{
		net.ParseIP("198.51.100.20"),
		net.ParseIP("192.0.2.10"),
	})
	if err != nil {
		t.Fatalf("2º Ensure: %v", err)
	}
	if reused {
		t.Error("IP novo fora dos SANs deveria regenerar (reused=false)")
	}
	hasIP := map[string]bool{}
	for _, ip := range parseCert(t, certPath).IPAddresses {
		hasIP[ip.String()] = true
	}
	if !hasIP["192.0.2.10"] {
		t.Errorf("cert regenerado sem o IP novo (tem %v)", hasIP)
	}
}

// TestEnsureRegeneraCorrompido: par truncado → regenera e volta a funcionar.
func TestEnsureRegeneraCorrompido(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _, err := Ensure(dir, nil)
	if err != nil {
		t.Fatalf("1º Ensure: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("lixo"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, reused, err := Ensure(dir, nil)
	if err != nil {
		t.Fatalf("Ensure com key corrompida: %v", err)
	}
	if reused {
		t.Error("par corrompido deveria regenerar (reused=false)")
	}
	// e o par regenerado precisa ser parseável
	_ = parseCert(t, certPath)
}

// TestEnsureRegeneraExpirado: certificado com NotAfter no passado → regenera.
// Simulado gravando-se um certificado vencido à mão.
func TestEnsureRegeneraExpirado(t *testing.T) {
	dir := t.TempDir()
	// gera um par válido para depois regravar o cert com validade vencida
	certPath, keyPath, _, err := Ensure(dir, nil)
	if err != nil {
		t.Fatalf("1º Ensure: %v", err)
	}
	cert := parseCert(t, certPath)
	// reutiliza a chave existente; emite cert vencido ontem
	keyPEM, _ := os.ReadFile(keyPath)
	block, _ := pem.Decode(keyPEM)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := *cert
	tmpl.NotBefore = now.Add(-48 * time.Hour)
	tmpl.NotAfter = now.Add(-time.Hour)
	tmpl.SerialNumber = big.NewInt(1)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, reused, err := Ensure(dir, nil)
	if err != nil {
		t.Fatalf("Ensure com cert vencido: %v", err)
	}
	if reused {
		t.Error("certificado vencido deveria regenerar (reused=false)")
	}
}

// TestDefaultDir: caminho termina em farchromectl.
func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if filepath.Base(dir) != "farchromectl" {
		t.Errorf("DefaultDir = %q, queria terminar em farchromectl", dir)
	}
}
