package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempConfig aponta o diretório de configuração para um diretório
// temporário exclusivo do teste.
func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "farchromectl", "config.json")
}

func TestLoadGeneratesAndPersistsToken(t *testing.T) {
	path := withTempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() primeira execução: erro inesperado: %v", err)
	}
	if !ValidToken(cfg.Token) {
		t.Fatalf("token gerado %q não tem 6 dígitos", cfg.Token)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("arquivo de configuração não foi criado: %v", err)
	}

	// Segunda execução deve reaproveitar o mesmo token.
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load() segunda execução: erro inesperado: %v", err)
	}
	if cfg2.Token != cfg.Token {
		t.Fatalf("token não foi reaproveitado: primeira=%q segunda=%q", cfg.Token, cfg2.Token)
	}
}

func TestLoadRegeneratesCorruptedFile(t *testing.T) {
	path := withTempConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{{{lixo"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() com arquivo corrompido: erro inesperado: %v", err)
	}
	if !ValidToken(cfg.Token) {
		t.Fatalf("token regenerado inválido: %q", cfg.Token)
	}
}

func TestGenerateTokenFormat(t *testing.T) {
	for range 100 {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidToken(tok) {
			t.Fatalf("token fora do formato: %q", tok)
		}
	}
}

func TestValidToken(t *testing.T) {
	cases := map[string]bool{
		"000000":  true,
		"123456":  true,
		"12345":   false,
		"1234567": false,
		"abc123":  false,
		"":        false,
	}
	for tok, want := range cases {
		if got := ValidToken(tok); got != want {
			t.Errorf("ValidToken(%q) = %v, queria %v", tok, got, want)
		}
	}
}
