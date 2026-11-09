// Package config carrega e persiste a configuração do farchromectl em
// ~/.config/farchromectl/config.json (respeitando $XDG_CONFIG_HOME e
// os.UserConfigDir no Windows/macOS).
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
)

// Config é o estado persistido entre execuções.
type Config struct {
	// Token é o PIN de 6 dígitos usado para autenticar clientes
	// (UI e extensão) no WebSocket.
	Token string `json:"token"`
}

// Path retorna o caminho absoluto do arquivo de configuração.
func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: não foi possível localizar o diretório de configuração do usuário: %w", err)
	}
	return filepath.Join(base, "farchromectl", "config.json"), nil
}

var tokenRe = regexp.MustCompile(`^[0-9]{6}$`)

// ValidToken informa se o token tem o formato esperado (6 dígitos).
func ValidToken(tok string) bool { return tokenRe.MatchString(tok) }

// GenerateToken gera um PIN aleatório de 6 dígitos via crypto/rand.
func GenerateToken() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", fmt.Errorf("config: falha ao gerar token: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Load devolve a configuração salva. Se não existir (ou estiver inválida),
// gera um novo token, persiste e o devolve. Na primeira execução o arquivo é
// criado com permissões 0600 no diretório 0700.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err == nil && ValidToken(cfg.Token) {
			return &cfg, nil
		}
		// arquivo corrompido ou token inválido: regenera abaixo.
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config: falha ao ler %s: %w", path, err)
	}

	cfg := &Config{}
	if cfg.Token, err = GenerateToken(); err != nil {
		return nil, err
	}
	if err := save(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func save(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: falha ao criar %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: falha ao serializar: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("config: falha ao gravar %s: %w", path, err)
	}
	return nil
}
