package main

import "testing"

// TestVersionOutput confere o formato da linha de --version. Sem carimbo de
// build (-ldflags "-X main.version=…") vale a versão default "dev"; o carimbo
// em si é exercitado pelo `make build` (verificado manualmente no CI/README).
func TestVersionOutput(t *testing.T) {
	want := "farchromectl dev"
	if got := versionOutput(); got != want {
		t.Fatalf("versionOutput = %q, queria %q", got, want)
	}
}
