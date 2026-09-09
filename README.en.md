<p align="center">
  <img src="assets/icon.svg" width="96" height="96" alt="share-clip icon">
</p>

# share-clip — LAN clipboard sharing

[![CI](https://github.com/micookie2/share-clip/actions/workflows/ci.yml/badge.svg)](https://github.com/micookie2/share-clip/actions/workflows/ci.yml)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

[简体中文](README.md) · **English**

A Windows/Linux clipboard synchronizer written in Go. Each client watches the
local clipboard and uploads every copy to the server; the server broadcasts it
live to all other clients and keeps a SQLite history. The server also serves a
small web page where you can browse history and re-push any entry to every
online client.

Supports **text** and **images** (PNG). **Copying files is not supported** and
is silently ignored.

```
   Windows box A                 Linux box B
 ┌──────────────┐              ┌──────────────┐
 │ shareclip-   │   copy /     │ shareclip-   │
 │ client       │◄────push────►│ client       │
 └──────┬───────┘  WebSocket   └──────┬───────┘
        │  ws://host:9000/ws          │
        └──────────────┬──────────────┘
                   ┌───▼────────────┐
                   │  shareclip-    │  always-on box (may also run a client)
                   │  server :9000  │
                   │  + SQLite      │
                   └───┬────────────┘
                       │ http://host:9000/
               ┌───────▼────────┐
               │   Web UI       │ browser: history / push
               └────────────────┘
```

## Behaviour

- When any client copies text or an image, the server immediately broadcasts to
  every other online client (sender excluded) and appends the entry to history
  (SQLite; last 500 entries kept by default, oldest trimmed automatically).
- Incoming broadcasts and web pushes are **written to the local clipboard only**
  and never re-uploaded — that is what stops A↔B ping-pong loops. Your own real
  re-copy still propagates normally.
- A copy carrying both text and an image is treated as **image first**. Windows
  enumerates clipboard formats directly; on X11 we fall back to a "try image only
  when text is empty" heuristic, so rare cases may miss the image.
- **Duplicate filtering**: if a copy is byte-identical (same kind, same bytes) to
  the **most recent** history entry, the server drops it — no write, no broadcast.
  Windows' clipboard sequence number reports an identical re-copy as a change,
  while Linux xclip/wl-paste polling (0.5–1s detection delay, tune with `-p`)
  cannot tell them apart; filtering on the server makes both platforms behave
  the same. Copying something different in between restores normal behaviour,
  and two machines copying the same text back to back still broadcast only the
  first one. Web "push" does not write history, so it is unaffected.
- A client does not push its current clipboard on startup, and a newly joined
  client does not receive backlog history — use the web page to push old entries
  on demand.
- Plaintext, no authentication: designed for a trusted LAN.
- Payload cap defaults to **32 MiB** (tune with `-m`); larger copies are ignored.

## Platforms and dependencies

| Platform | Dependency | Notes |
|----------|------------|-------|
| Windows 10/11 | none (pure Go syscall) | images read via CF_DIB, converted with our own DIB↔PNG codec |
| Linux (X11) | `xclip` | `apt install xclip` / `dnf install xclip` |
| Linux (Wayland) | `wl-clipboard` | e.g. `apt install wl-clipboard` |

Clients must run inside a graphical session (`DISPLAY` or `WAYLAND_DISPLAY`) and
exit with an error otherwise. The server needs no desktop, so it is happy on an
always-on box or NAS.

## Build

Requires Go 1.25+ (Go 1.22-style routing and newer stdlib APIs). Third-party
dependencies: WebSocket library `github.com/coder/websocket` and the pure-Go
SQLite driver `modernc.org/sqlite`.

```bash
make build          # bin/shareclip-server + bin/shareclip-client (native)
make build-windows  # cross-compile bin/*.exe (Windows amd64)
# or manually:
go build -o shareclip-server ./cmd/shareclip-server
GOOS=windows GOARCH=amd64 go build -o shareclip-client.exe ./cmd/shareclip-client
```

You can also install straight from the module path (binary name = directory name):

```bash
go install github.com/micookie2/share-clip/cmd/shareclip-server@latest
go install github.com/micookie2/share-clip/cmd/shareclip-client@latest
# binaries land in $(go env GOPATH)/bin
```

No prebuilt binaries are committed (`bin/` is gitignored). The commands above
were verified with Go 1.25.6 on linux/amd64 and windows/amd64.

## Usage

```bash
# 1) start the server on the always-on box (port 9000, shareclip.db in cwd)
./shareclip-server

# 2) start a client on every machine that should share its clipboard
./shareclip-client -s 192.168.1.10:9000

# 3) just copy and paste as usual. Optionally open http://192.168.1.10:9000/
#    to browse history and re-push an entry to all online clients.
```

Every option has a single-letter short form, and `-h` prints the full list.

### Server options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-a` | `--addr` | `:9000` | listen address; HTTP and WebSocket share one port |
| `-d` | `--db` | `shareclip.db` | SQLite history database path |
| `-l` | `--history-limit` | `500` | how many history entries to keep |
| `-m` | `--max-payload` | `33554432` (32 MiB) | max bytes per clipboard entry |
| `-q` | `--quiet` | false | less logging |
| `-v` | `--version` | — | print version and exit |

### Client options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-s` | `--server` | (required) | server address `host:port` |
| `-n` | `--name` | hostname | machine name shown elsewhere |
| `-p` | `--poll` | `1000` | clipboard polling interval in ms (Linux only) |
| `-m` | `--max-payload` | `33554432` (32 MiB) | max bytes per clipboard entry |
| `-q` | `--quiet` | false | less logging |
| `-v` | `--version` | — | print version and exit |

Short and long forms are interchangeable (`-s`, `-server` and `--server` all
work; values can be `-s host:port` or `-s=host:port`), so existing autostart
entries keep working unchanged.

## Web UI

`http://<server>:9000/` (listens on all interfaces by default):

- **Live updates**: the page holds a Server-Sent Events connection
  (`/api/events`) — new copies appear at the top instantly (briefly highlighted),
  node presence updates immediately, and reconnects backfill anything missed.
  No manual refresh anywhere.
- **Node counter in the header**: shows how many clients are connected (green
  indicator when online), click for the list; the browser tab title mirrors it as
  `(N online)`.
- History list (newest first, 50 per page + "load more"): text entries can be
  expanded and copied, image thumbnails open in a lightbox. Timestamps render as
  `YYYY-MM-DD HH:mm:ss` in your browser timezone (hover for the server timestamp).
- Each entry has a "push to all online clients" button that writes the content
  into **every** currently online client regardless of origin, with a toast for
  the result.
- "Clear" wipes the database; other open pages clear in real time.
- Flat, quiet UI: white cards with 1px borders, solid accent colour (no
  gradients, no drop shadows), automatic light/dark theme, mobile friendly.

APIs: `GET /api/status` (online node snapshot), `GET /api/events` (SSE stream of
`status` / `clip` / `cleared`).

## Layout

```
assets/                 brand icon (icon.svg is the source; PNG/ICO and the
                        Windows .syso resources are generated from it)
cmd/shareclip-server/   server entry point
cmd/shareclip-client/   client entry point
internal/agent/         client logic: clipboard watch, send/receive, reconnect
internal/clipboard/     cross-platform clipboard abstraction + watcher
  clipboard_windows.go  Win32: CF_UNICODETEXT / CF_DIB (stdlib syscall)
  clipboard_linux.go    Linux: xclip / wl-clipboard
internal/dib/           pure-Go DIB↔PNG codec (Windows images)
internal/cli/           options with long name + single-letter alias sharing one var
internal/protocol/      message framing (JSON header + binary payload)
internal/server/        WS hub, broadcast, SSE events, web API, embedded page
internal/store/         SQLite history (trim / pagination / content reads)
```

## Tests

```bash
go test -count=1 ./...   # protocol / DIB / watcher / store / e2e broadcast+push
```

The e2e test drives real WebSocket connections: broadcast to everyone but the
sender, image payloads, history persistence, web content reads and pushes, and
clearing history.

## Known limitations (intentional v1 scope)

- Plaintext, unauthenticated — trusted LAN only. Use a VPN or terminate TLS
  yourself if the traffic leaves one.
- On Linux, polling latency means an identical re-copy cannot be detected (see
  behaviour above).
- X11 cannot enumerate clipboard formats cheaply, so an image copy that also
  carries text may occasionally be missed.
- Web push always targets all online clients; no per-machine targeting yet.
- Copies made while disconnected are not replayed after reconnecting — just copy
  again.
- History is browse-only; formats beyond text/image (file lists, rich HTML) are
  not handled.

## Roadmap

- File transfer, TLS and simple password auth
- System tray client
- Per-machine push, multi-select, history export/search
- Compression (lossless PNG of a screenshot can be large)

## License

[MIT](LICENSE).
