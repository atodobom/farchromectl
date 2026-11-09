// Package certs gera e persiste o certificado TLS self-signed usado pelo
// farchromectl com --tls (PWA instalável no celular).
//
// Características deliberadas:
//   - ECDSA P-256 (rápido para gerar, amplamente aceito);
//   - SANs: DNS "localhost" e "farchromectl.local" + IP 127.0.0.1 + todos os
//     IPs não-loopback da máquina (o celular pode abrir por IP ou por mDNS);
//   - validade de 825 dias — o máximo que iOS 13+ aceita para certificados
//     confiados manualmente (perfis), com folga para Android/Linux;
//   - par persistido em ~/.config/farchromectl/{cert,key}.pem (0600) e
//     REUTILIZADO nas execuções seguintes: o celular não deve precisar
//     confiar num certificado novo a cada restart do servidor;
//   - regenera somente quando o par está corrompido, expirado ou os IPs
//     locais mudaram (novo IP fora dos SANs quebraria HTTPS por IP).
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/antonio/farchromectl/internal/mdns"
)

const (
	certName = "cert.pem"
	keyName  = "key.pem"

	// validity é a validade do certificado gerado: 825 dias (limite aceito
	// pelo iOS para confiança manual via perfil).
	validity = 825 * 24 * time.Hour

	// dnsNames são os SANs DNS fixos além dos IPs.
	localhost  = "localhost"
	mdnsHost   = "farchromectl.local"
	commonName = "farchromectl"
)

// DefaultDir devolve o diretório onde o par é persistido
// (~/.config/farchromectl, respeitando $XDG_CONFIG_HOME).
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("certs: não foi possível localizar o diretório de configuração do usuário: %w", err)
	}
	return filepath.Join(base, "farchromectl"), nil
}

// MachineIPs devolve os IPs não-loopback da máquina (mesma regra do banner e
// do mDNS) — base dos SANs de IP.
func MachineIPs() []net.IP { return mdns.NonLoopbackIPs() }

// Ensure garante um par cert/key válido em dir para os IPs dados (mais
// 127.0.0.1 e os SANs DNS fixos). Reutiliza o par salvo quando ainda é válido
// e cobre todos os IPs requeridos; regenera quando corrompido, expirado ou
// quando algum IP requerido não está nos SANs (IP da máquina mudou).
// Devolve os caminhos do certificado/chave e reused=true quando o par
// existente foi mantido.
func Ensure(dir string, ips []net.IP) (certPath, keyPath string, reused bool, err error) {
	certPath = filepath.Join(dir, certName)
	keyPath = filepath.Join(dir, keyName)

	ok, err := reusable(certPath, keyPath, ips, time.Now())
	if err != nil {
		return "", "", false, err
	}
	if ok {
		return certPath, keyPath, true, nil
	}

	if err := generate(certPath, keyPath, ips); err != nil {
		return "", "", false, err
	}
	return certPath, keyPath, false, nil
}

// reusable informa se o par salvo em certPath/keyPath serve para os IPs
// requeridos: parseia, casa chave↔certificado, confere validade e cobertura
// de SANs. Qualquer problema devolve false (regenera), exceto erros de I/O
// que não sejam "arquivo inexistente".
func reusable(certPath, keyPath string, wantIPs []net.IP, now time.Time) (bool, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("certs: falha ao ler %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("certs: falha ao ler %s: %w", keyPath, err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, nil // corrompido/incompatível: regenera
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, nil
	}
	if now.After(cert.NotAfter) || now.Before(cert.NotBefore.Add(-24*time.Hour)) {
		return false, nil // expirado (ou janela inválida): regenera
	}

	// cobertura de SANs: DNSs fixos + 127.0.0.1 + IPs requeridos.
	hasDNS := map[string]bool{}
	for _, d := range cert.DNSNames {
		hasDNS[d] = true
	}
	if !hasDNS[localhost] || !hasDNS[mdnsHost] {
		return false, nil
	}
	hasIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		hasIP[ip.String()] = true
	}
	if !hasIP["127.0.0.1"] {
		return false, nil
	}
	for _, ip := range wantIPs {
		if !hasIP[ip.String()] {
			return false, nil // IP novo (DHCP etc.): regenera
		}
	}
	return true, nil
}

// generate cria o par ECDSA P-256 self-signed e grava em certPath/keyPath
// (0600, diretório 0700). A chave é gravada primeiro: um cert.pem sem chave
// legível cai no caminho "corrompido → regenera" do próximo Ensure.
func generate(certPath, keyPath string, ips []net.IP) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("certs: falha ao gerar chave ECDSA: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("certs: falha ao gerar serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour), // tolerância de clock
		NotAfter:     now.Add(validity),
		// CertSign/IsCA: o certificado é self-signed e precisa valer como
		// "CA" para ser instalável no armazenamento de CAs do usuário no
		// Android (Fase 4 — confiança manual no celular).
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{localhost, mdnsHost},
	}
	seen := map[string]bool{"127.0.0.1": true}
	tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	for _, ip := range ips {
		if ip == nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("certs: falha ao criar certificado: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("certs: falha ao serializar chave: %w", err)
	}

	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("certs: falha ao criar %s: %w", dir, err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return fmt.Errorf("certs: falha ao gravar %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return fmt.Errorf("certs: falha ao gravar %s: %w", certPath, err)
	}
	return nil
}
