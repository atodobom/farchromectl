BIN := bin/farchromectl
VERSION ?= $(shell git describe --tags --always --dirty)

.PHONY: build install test vet run clean e2e e2e-tls

build:
	@echo "compilando $(BIN) — versão $(VERSION)"
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/farchromectl

install: build
	install -Dm755 $(BIN) $(HOME)/.local/bin/farchromectl

test:
	go test ./...

vet:
	go vet ./...

# E2E com Chrome real: servidor + extensão (arquivos de produção) + mídia
# local, validado via CDP. Requer google-chrome/chromium instalado.
e2e: build
	go run ./e2e -bin $(BIN)

# E2E em modo TLS: servidor com --tls (HTTPS verificado contra o cert de
# teste, cert isolado via XDG_CONFIG_HOME temporário), Chrome com
# --ignore-certificate-errors, UI via https/wss, service worker pronto e
# extensão conectando pelo listener HTTP loopback (porta principal +1).
e2e-tls: build
	go run ./e2e -bin $(BIN) -tls

run: build
	$(BIN)

clean:
	rm -rf bin
