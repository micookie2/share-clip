GO ?= go
BIN := bin

# ── 构建信息 ────────────────────────────────────────────────────────────────
# 编译期通过 -ldflags -X 写进 internal/buildinfo，启动日志、-v 与 Web 页面
# 都从这里取值。没有 make（直接 go build）时自动回退到 Go 自带的 VCS 标记。
BUILDINFO := github.com/micookie2/share-clip/internal/buildinfo
VERSION   ?= $(shell git describe --tags --exact-match HEAD 2>/dev/null)
COMMIT    ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DIRTY     := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true)

# 注意：GNU make 的 ifdef 对「定义但为空」的变量也会判真，故用 ifneq 显式判空，
# 免得 git describe 没有 tag 时把版本号覆盖成空字符串。
LDFLAGS := -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).BuildTime=$(BUILD_TIME)
ifneq ($(VERSION),)
LDFLAGS += -X $(BUILDINFO).Version=$(VERSION)
endif
ifneq ($(DIRTY),)
LDFLAGS += -X $(BUILDINFO).Dirty=true
endif

.PHONY: all build build-windows test vet fmt icon icon-windows clean run-server version

# 默认目标：一次产出全部交付物 —— 本机版 + Windows amd64 版（bin/ 下 4 个文件）。
# 只想出某一平台时用下面的细分目标：make build / make build-windows。
all: build build-windows

# 当前平台（本机）的二进制。
build:
	@mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/shareclip-server ./cmd/shareclip-server
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/shareclip-client ./cmd/shareclip-client

# Windows amd64 二进制，可从 Linux/macOS 交叉编译，也可在 Windows 本机编译。
build-windows:
	@mkdir -p $(BIN)
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/shareclip-server.exe ./cmd/shareclip-server
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/shareclip-client.exe ./cmd/shareclip-client

# 打印本次会注入的构建信息（Release 前自检用）。
version:
	@echo "version    = $(if $(VERSION),$(VERSION),<buildinfo 默认值>)"
	@echo "commit     = $(COMMIT)$(if $(DIRTY), (+dirty))"
	@echo "build time = $(BUILD_TIME) UTC"
	@echo "ldflags    = $(LDFLAGS)"

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
