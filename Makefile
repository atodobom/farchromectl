BIN := bin/farchromectl
VERSION ?= $(shell git describe --tags --always --dirty)

.PHONY: build install test vet run clean

build:
	@echo "compilando $(BIN) — versão $(VERSION)"
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/farchromectl

install: build
	install -Dm755 $(BIN) $(HOME)/.local/bin/farchromectl

test:
	go test ./...

vet:
	go vet ./...

run: build
	$(BIN)

clean:
	rm -rf bin
