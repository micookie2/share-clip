GO ?= go
BIN := bin

.PHONY: all build build-windows test vet fmt icon icon-windows clean run-server

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
	gofmt -w cmd internal assets

# Regenerate the raster icons (PNG + .ico) from assets/icon.svg.
# Needs: pip install cairosvg pillow
icon:
	python3 assets/generate.py

# Refresh the Windows .exe icon resources from assets/icon.ico.
# Needs: go install github.com/akavel/rsrc@latest
# The generated .syso files are committed, so a normal build never needs rsrc.
icon-windows:
	rsrc -ico assets/icon.ico -arch amd64 -o cmd/shareclip-server/rsrc_windows_amd64.syso
	rsrc -ico assets/icon.ico -arch amd64 -o cmd/shareclip-client/rsrc_windows_amd64.syso

run-server:
	$(GO) run ./cmd/shareclip-server

clean:
	rm -rf $(BIN) shareclip.db shareclip.db-*
