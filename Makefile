GO ?= go
BIN := bin

.PHONY: all build test vet fmt clean run-server

all: build

build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/shareclip-server ./cmd/shareclip-server
	$(GO) build -o $(BIN)/shareclip-client ./cmd/shareclip-client

# Windows binaries can be cross-compiled from Linux/macOS or built natively.
build-windows:
	@mkdir -p $(BIN)
	GOOS=windows GOARCH=amd64 $(GO) build -o $(BIN)/shareclip-server.exe ./cmd/shareclip-server
	GOOS=windows GOARCH=amd64 $(GO) build -o $(BIN)/shareclip-client.exe ./cmd/shareclip-client

test:
	$(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

run-server:
	$(GO) run ./cmd/shareclip-server

clean:
	rm -rf $(BIN) shareclip.db shareclip.db-*
