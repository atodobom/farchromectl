BIN := bin/farchromectl

.PHONY: build install test vet run clean

build:
	go build -o $(BIN) ./cmd/farchromectl

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
