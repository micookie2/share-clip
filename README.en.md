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

A client runs in one of two ways: **run it without `-s` (double-click)** and it
enters desktop mode — a resident system-tray icon plus a local settings/log page
opened in your default browser; **pass `-s`** (or add `--console`) and it keeps
the original console behaviour, logging to the terminal, which is what scripts
and autostart entries want.

Supports **text**, **rich text (HTML)** and **images** (PNG). **Copying files
is not supported** and is silently ignored. Rich-text sync carries both a
plain-text and an HTML rendition, so pasting into a browser/Office editor keeps
its formatting while pasting into a plain-text field still yields plain text.

```
   Windows box A                 Linux box B
 ┌──────────────┐              ┌──────────────┐
 │ shareclip-   │   copy /     │ shareclip-   │
 │ client       │◄────push────►│ client       │
 │ tray + web UI│              │ tray + web UI│
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
  re-copy still propagates normally. Echo suppression is keyed to the content the
  local clipboard *actually presents* after the write, so even when Windows
  stores a received PNG as a CF_DIB bitmap and hands it back re-encoded with
  different bytes, the machine does not mistake it for a fresh local copy.
- A copy carrying both text and an image is treated as **image first**. Windows
  and Linux both enumerate the formats the clipboard owner actually offers
  (CF_DIB on Windows; xclip `TARGETS` / `wl-paste --list-types` on Linux): an
  image format wins over text, and non-PNG encodings (JPEG/GIF/BMP/TIFF/WebP)
  are converted to PNG before sharing.
- **Own-copy protection**: if a peer sends this machine's own recent copy straight
  back — either unchanged or downgraded to plain text — the client ignores it
  instead of overwriting the local clipboard. Otherwise a rich copy could turn
  into plain text locally right after copying, because the echoed plain text
  replaced the HTML on the clipboard.
- **Duplicate filtering**: if a copy duplicates the **most recent** history entry,
  the server drops it — no write, no broadcast. Text duplicates mean byte-identical
  payloads (rich text must match in both HTML and plain text); image duplicates
  mean PNGs that *decode to the same picture*, so the
  same image repackaged in a different PNG encoding (e.g. by a Windows CF_DIB
  bitmap round-trip) is still broadcast only once — clients never receive the
  same picture twice. Windows (clipboard sequence number) and Linux/X11 (XFixes
  copy events) both report an identical re-copy as a change; Linux/Wayland only
  has polling and cannot tell them apart; filtering on the server makes both
  platforms behave the same. Copying something different in between
  restores normal behaviour, and two machines copying the same content back to
  back still broadcast only the first one. Web "push" does not write history, so
  it is unaffected.
- A client does not push its current clipboard on startup, and a newly joined
  client does not receive backlog history — use the web page to push old entries
  on demand.
- **Desktop mode**: running without `-s` starts desktop mode (system tray + local
  UI) and saves settings such as the server address in the user config directory
  for the next start; `--console`, or passing `-s` on the command line, keeps the
  console behaviour. Pausing sync disconnects from the server: nothing is sent or
  received while paused, and resuming reconnects immediately (copies made while
  paused are not replayed).
- **One log line per event**: clipboard text, machine names and errors are
  escaped before printing (`\n`, `\t` appear literally), so grep works.
- **Message-level logging**: the server prints every frame it receives
  (`[recv] …`) and every push it sends out (`[push] … -> N client(s)`), and the
  client prints what it sends (`[send] …`) and receives (`[recv] …`) — hello,
  welcome, joined/left presence, clip broadcasts and web pushes all appear
  (heartbeats use WebSocket control-frame pings, so they never show up here),
  with a short text preview per clip. `-q` silences all of it.
- Plaintext, no authentication: designed for a trusted LAN.
- Payload cap defaults to **32 MiB** (tune with `-m`); larger copies are ignored.

## Platforms and dependencies

| Platform | Dependency | Notes |
|----------|------------|-------|
| Windows 10/11 | none (pure Go syscall) | images read via CF_DIB, converted with our own DIB↔PNG codec |
| Linux (X11) | `xclip` | copies detected via XFixes events (present on virtually every modern X server), polling only as fallback; `apt install xclip` |
| Linux (Wayland) | `wl-clipboard` | wl-clipboard 2.x (`--list-types`) works best; no event channel on GNOME etc., so it polls at the `-p` interval; e.g. `apt install wl-clipboard` |
| Tray (Windows / Linux) | none (`fyne.io/systray` is pure Go) | Windows uses the system `Shell_NotifyIcon`; Linux speaks StatusNotifierItem over the D-Bus session bus and needs a panel that supports it (KDE, XFCE, MATE, Cinnamon, GNOME + an AppIndicator extension, …) |

A client's clipboard watching needs a graphical session (`DISPLAY` or
`WAYLAND_DISPLAY`): in console mode it exits with an error without one, while
desktop mode shows "local environment problem" in the UI/tray and retries with
backoff (no client restart needed once the dependency is installed). The server
needs no desktop, so it is happy on an always-on box or NAS.

The tray and local UI add no new system dependency: Windows and Linux both use
pure-Go `fyne.io/systray` v1.12.2 (no CGO, no GTK, no extra build tags). The
Linux tray works over the freedesktop StatusNotifierItem protocol on the D-Bus
session bus and needs a desktop panel that supports it (KDE, XFCE, MATE,
Cinnamon, GNOME with an AppIndicator extension, …); when no session bus is
detected the client still runs, just without a tray icon, and you quit from the
local UI's "quit client" button.

## Build

Requires Go 1.25+ (Go 1.22-style routing and newer stdlib APIs). Third-party
dependencies: WebSocket library `github.com/coder/websocket`, the pure-Go
SQLite driver `modernc.org/sqlite`, and the tray library `fyne.io/systray`
v1.12.2 (pure Go on both Windows and Linux — no CGO/GTK, no extra build tags).

```bash
make                # everything at once: native + Windows amd64 (4 files in bin/)
make build          # native only: bin/shareclip-server + bin/shareclip-client
make build-windows  # Windows only: cross-compile bin/*.exe (Windows amd64)
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

## Version and build metadata

`internal/buildinfo` holds the metadata; `make build` / `make build-windows`
stamp it in at link time with `-ldflags -X`:

| Field | Source | Notes |
| --- | --- | --- |
| `Version` | `git describe --tags --exact-match HEAD` | the tag on HEAD, else the default in the source |
| `Commit` | `git rev-parse --short=7 HEAD` | 7-character short SHA |
| `BuildTime` | `date -u +%Y-%m-%dT%H:%M:%SZ` | the link instant (UTC), shown in local time |
| `Dirty` | `git status --porcelain` | appends `+dirty` when the tree has local changes |

```bash
make version        # show what the next build would inject
```

Three places consume it:

```bash
$ ./bin/shareclip-server -v
v0.1.0 (commit 0be9e27, built 2026-09-09 13:00:11 +0800, go1.25.6 linux/amd64)

$ ./bin/shareclip-server
2026-09-09 13:00:20 share-clip server v0.1.0 启动（commit 0be9e27，构建于 2026-09-09 13:00:11 +0800）
```

The web page shows `· v0.1.0` in the header subtitle (hover for commit and build
time) plus a full line at the bottom, read from `GET /api/version`.

`go build` / `go install` bypass make, so no `-ldflags` are applied: `Commit`
and the build instant then fall back to the VCS stamps Go embeds automatically
(commit SHA and commit time), and the version keeps its in-source default.

## Usage

```bash
# 1) start the server on the always-on box (port 9000, shareclip.db in cwd)
./shareclip-server

# 2) start a client on every machine that should share its clipboard, either way:

#    2a) desktop mode (double-click, or just run it with no arguments): the tray
#        icon stays resident and the local settings page opens in your browser
#        (default http://127.0.0.1:9210). Fill in the server address there the
#        first time. Same on Windows and Linux.
./shareclip-client

#    2b) console mode (scripts, autostart): pass the server address explicitly;
#        logs go to the terminal, no tray, no page. Passing -s or --console both
#        take this path.
./shareclip-client -s 192.168.1.10:9000
./shareclip-client --console            # address comes from the saved desktop config

# 3) just copy and paste as usual. Optionally open http://192.168.1.10:9000/
#    to browse history and re-push an entry to all online clients.
```

Every option has a single-letter short form, and `-h` prints the full list. The
server also has `install` / `uninstall` subcommands (see "Installing as a
systemd service" below).

### Server options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-a` | `--addr` | `:9000` | listen address; HTTP and WebSocket share one port |
| `-d` | `--db` | `shareclip.db` | SQLite history database path |
| `-l` | `--history-limit` | `500` | how many history entries to keep |
| `-m` | `--max-payload` | `33554432` (32 MiB) | max bytes per clipboard entry |
| `-q` | `--quiet` | false | less logging |
| `-v` | `--version` | — | print version and build metadata (commit, build time, Go/platform) and exit |

### Client options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-s` | `--server` | — | server address `host:port`; omitting `-s` defaults to desktop mode (set the address in the UI), giving it defaults to console mode (`-g`/`-c` override the default) |
| `-g` | `--gui` | false | force desktop mode (tray + local UI), e.g. `--gui -s 1.2.3.4:9000` |
| `-c` | `--console` | false | force console mode; without `-s` it reuses the server address saved by the desktop UI |
| `-n` | `--name` | hostname | machine name shown elsewhere |
| `-p` | `--poll` | `1000` | fallback monitoring interval in ms: X11 is event driven (XFixes) and only polls when events are unavailable; Wayland (no event channel) polls at this interval |
| `-m` | `--max-payload` | `33554432` (32 MiB) | max bytes per clipboard entry |
| — | `--ui-addr` | `127.0.0.1:9210` | desktop-mode local UI listen address (loopback only) |
| — | `--no-open` | false | do not auto-open the browser in desktop mode |
| `-q` | `--quiet` | false | less logging |
| `-v` | `--version` | — | print version and build metadata (commit, build time, Go/platform) and exit |

Short and long forms are interchangeable (`-s`, `-server` and `--server` all
work; values can be `-s host:port` or `-s=host:port`), so existing autostart
entries keep working unchanged. `-g/--gui` and `-c/--console` only force the run
mode and change nothing about how the other options are written.

### Desktop mode (tray + local UI)

Running without `-s` (double-click works) enters desktop mode; `-g/--gui` forces
it, e.g. `shareclip-client --gui -s 1.2.3.4:9000` (values given on the command
line win over the saved config).

- **Tray + browser page**: the tray icon stays resident and the local page opens
  in your default browser at `http://127.0.0.1:9210` (`--ui-addr` changes it; it
  listens on loopback only, and `--no-open` skips opening the browser). Closing
  the page does not stop syncing.
- **What the page does**: edit the server address, machine name, poll interval
  and payload cap (saving applies the new settings and reconnects immediately);
  show live connection status (phase, current server, reconnect countdown) and
  live logs (pushed over SSE, clearable, auto-scroll); the footer shows the
  config and log file paths, whether the tray is available and the version/build
  metadata, plus a "quit client" button.
- **Tray menu**: a disabled status line (e.g. `已连接：192.168.1.10:9000`,
  "connected"), `打开设置与日志` (open settings and logs), an
  `启用同步`/`暂停同步` (enable/pause sync) checkbox, and `退出` (quit). Pausing
  disconnects from the server: nothing is sent or received while paused, and
  resuming reconnects immediately.
- **Persisted settings**: Windows `%AppData%\share-clip\config.json`, Linux
  `~/.config/share-clip/config.json` (XDG). Fields: `server`, `name`, `pollMs`,
  `maxPayload`. Desktop mode also writes a log file `client.log` next to it,
  rotated to `client.log.old` at 2 MiB — a tray app has no visible console, so
  this file is the on-disk record.
- **Single instance**: a second double-click first probes the fixed port
  `127.0.0.1:9210` for a running share-clip client and just re-opens its page
  instead of starting a second process; if something else holds that port the UI
  falls back to a random port (noted in the log), and a second double-click then
  does start a second process.
- **Windows**: the client still builds as a console program (`-v` and running it
  from a terminal keep working), and desktop mode hides the console window the
  system allocates on double-click; launched from an existing terminal, that
  terminal is left visible.
- **Linux**: the tray uses the freedesktop **StatusNotifierItem** protocol over
  the D-Bus session bus (via `fyne.io/systray`), so it needs a desktop panel that
  supports it (KDE, XFCE, MATE, Cinnamon, GNOME with an AppIndicator extension,
  …). When no session bus is detected the client still runs, just without a tray
  icon (the log says no usable system tray was detected), and you quit with the
  page's "quit client" button. No extra system packages are needed to build.
- **Security**: the UI binds loopback only, rejects non-loopback `Host` headers
  (DNS-rebinding guard) and requires the custom `X-Requested-With: share-clip`
  header on every write request (CSRF guard), so it adds no network exposure.
- A running client serves `GET /api/health`, `/api/state`, `/api/logs` and
  `/api/events` (SSE), plus `POST /api/config`, `/api/reconnect`, `/api/pause`,
  `/api/logs/clear` and `/api/quit` on that local UI.

### Installing as a systemd service

```bash
# system scope (needs root): /etc/systemd/system/share-clip.service, starts at boot
sudo install -m755 bin/shareclip-server /usr/local/bin/shareclip-server  # fixed path first
sudo shareclip-server install
sudo shareclip-server install --no-start   # enable only, do not start now

# user scope (no root): ~/.config/systemd/user/share-clip.service
shareclip-server install --user
loginctl enable-linger <user>   # to survive logout / start without a login session

# print the unit file and the commands without touching anything
shareclip-server install --dry-run

# uninstall: stop + disable + delete the unit + reload; --purge also deletes the data dir
sudo shareclip-server uninstall
shareclip-server uninstall --user --purge
```

- `install` writes the unit file, then runs `systemctl daemon-reload` and
  `systemctl enable --now <unit>` (with `--no-start`, only `enable`); user-scope
  commands always carry `--user`.
- Data directory: system scope `/var/lib/share-clip` (created and owned by the
  unit's `StateDirectory=`; with the default `DynamicUser=yes` it actually lives
  under `/var/lib/private/share-clip`, with `/var/lib/share-clip` a symlink to
  it), user scope `~/.local/share/share-clip` (the unit creates it with
  `ExecStartPre=/bin/mkdir -p`). The database defaults to `shareclip.db` inside
  that directory.
- `install`'s `-a/-d/-l/-m` decide the listen address, database path, history
  limit and payload cap written into `ExecStart` (same defaults as a normal run,
  except the database path, which defaults into the data directory instead of the
  current directory). **Any other server flag** — `-q` today, and whatever gets
  added later — goes in through the repeatable `--exec-arg`, e.g.
  `sudo shareclip-server install --exec-arg=-q --exec-arg=-l --exec-arg=1000`,
  which produces
  `ExecStart=…/shareclip-server -a :9000 -d … -l 500 -m 33554432 -q -l 1000`
  (a repeated scalar flag wins, so this also overrides the defaults written
  before it).
- To change more than the startup arguments (environment variables, resource
  limits, dependencies, …), use systemd's own override instead of editing the
  unit file: `sudo systemctl edit share-clip` creates
  `/etc/systemd/system/share-clip.service.d/override.conf`, where you write a
  `[Service]` section. To replace `ExecStart` you must first clear the inherited
  value with an empty `ExecStart=` line and then add the new `ExecStart=…`
  (`Type=simple` rejects two `ExecStart` lines). The drop-in lives in its own
  directory, so re-running `install --force` does not clobber it.
- The unit always sets `Restart=always`, `RestartSec=2`, journald logging,
  `After/Wants=network-online.target`, and hardening (`NoNewPrivileges`,
  `PrivateTmp`, `ProtectSystem=full`; the system scope adds `ProtectHome=true`).
- Troubleshooting: `systemctl status share-clip`, `journalctl -u share-clip -f`;
  add `--user` for the user scope (`systemctl --user status share-clip`,
  `journalctl --user -u share-clip -f`).

**Install from a fixed path**: `install` uses the path of the executable that is
*running right now*. A `go run` temp binary is rejected outright (it will not
exist after a reboot), and a binary under your home directory (e.g.
`~/go/bin/shareclip-server`) does not work for the system scope either, because
the system unit sets `ProtectHome=true` and cannot see home directories. Build
first, then `sudo install -m755 bin/shareclip-server /usr/local/bin/`, and run
`install` from that path.

#### install options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-a` | `--addr` | `:9000` | listen address written into `ExecStart` |
| `-d` | `--db` | `shareclip.db` in the data directory | SQLite database path; empty derives it from the scope |
| `-l` | `--history-limit` | `500` | how many history entries to keep |
| `-m` | `--max-payload` | `33554432` (32 MiB) | max bytes per clipboard entry |
| `-u` | `--user` | false | install a user service (`~/.config/systemd/user`, no root) |
| `-n` | `--name` | `share-clip` | unit name (without `.service`) |
| — | `--run-as` | empty (`DynamicUser=yes`) | run the system service as that user; ignored for the user scope |
| — | `--exec-arg` | empty | extra startup arguments appended to `ExecStart`, repeatable (e.g. `--exec-arg=-q`); future server flags go here too, and a repeated scalar flag wins over an earlier one |
| — | `--unit-dir` | by scope | override the unit file directory |
| — | `--no-start` | false | enable only, do not start now |
| `-f` | `--force` | false | overwrite an existing unit file |
| — | `--dry-run` | false | only print the unit file and the commands to run |

#### uninstall options

| Short | Long | Default | Description |
|-------|------|---------|-------------|
| `-u` | `--user` | false | uninstall the user service (`~/.config/systemd/user`) |
| `-n` | `--name` | `share-clip` | unit name (without `.service`) |
| — | `--unit-dir` | by scope | override the unit file directory |
| — | `--purge` | false | also delete the data directory (the database) |
| — | `--dry-run` | false | only print the files to delete and the commands to run |

## Web UI (server admin page)

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
- The server **version and build time** appear in the header and at the bottom
  of the list (stamped in at compile time, see "Version and build metadata").
- Flat, quiet UI: white cards with 1px borders, solid accent colour (no
  gradients, no drop shadows), automatic light/dark theme, mobile friendly.

APIs: `GET /api/status` (online node snapshot), `GET /api/version` (version,
commit, build time, Go version/platform), `GET /api/events` (SSE stream of
`status` / `clip` / `cleared`).

## Layout

```
assets/                 brand icon (icon.svg is the source; PNG/ICO and the
                        Windows .syso resources are generated from it)
cmd/shareclip-server/   server entry point (including install / uninstall)
cmd/shareclip-client/   client entry point: picks console/desktop mode and wires
                        up tray, local UI and agent
internal/agent/         client logic: clipboard watch, send/receive, reconnect
                        (including connection-status callbacks)
internal/appconfig/     client persisted settings: config.json path, defaults,
                        load/save, server-address normalisation
internal/clipboard/     cross-platform clipboard abstraction + watcher
  clipboard_windows.go  Win32: CF_UNICODETEXT / CF_HTML / CF_DIB (stdlib syscall)
  clipboard_linux.go    Linux: xclip / wl-clipboard; format listing, image
                        first, rich text, non-PNG → PNG conversion
  x11owner_linux.go     X11: native CLIPBOARD selection owner (text/plain + text/html)
  x11watch_linux.go     X11: XFixes copy-event listener (falls back to polling)
internal/buildinfo/     version/commit/build time (-ldflags; logs, -v, web UI)
internal/dib/           pure-Go DIB↔PNG codec (Windows images)
internal/cli/           options with long name + single-letter alias sharing one var
internal/clientsvc/     client service: config + agent lifecycle + status + log ring
internal/clientui/      desktop-mode local UI: loopback-only HTTP service
                        (DNS-rebinding / CSRF guards)
  webui/index.html      settings/status/log page (embedded, opened in the browser)
internal/logx/          logging entry point: one event always occupies one line
                        (desktop mode also writes client.log)
internal/protocol/      message framing (JSON header + binary payload)
internal/server/        WS hub, broadcast, SSE events, web API, embedded page
internal/store/         SQLite history (trim / pagination / content reads)
internal/systemd/       systemd unit generation + install/uninstall (pure
                        functions plus a replaceable command runner)
internal/tray/          tray icon and menu (fyne.io/systray)
```

## Tests

```bash
go test -count=1 ./...   # protocol / DIB / watcher / store / client service /
                         # tray / systemd / e2e broadcast+push
```

The e2e test drives real WebSocket connections: broadcast to everyone but the
sender, image payloads, history persistence, web content reads and pushes, and
clearing history. The tray menu model, systemd unit generation and install flow
(the command runner is replaced, so systemctl is never called for real), client
config and service lifecycle all have their own unit tests and need neither a
graphical session nor systemd.

## Known limitations (intentional v1 scope)

- Plaintext, unauthenticated — trusted LAN only. Use a VPN or terminate TLS
  yourself if the traffic leaves one.
- Desktop mode's UI is a browser page, not a native window: reading the logs or
  changing settings means keeping that page open (closing it does not stop
  syncing).
- The Linux tray in desktop mode relies on the StatusNotifierItem protocol on the
  D-Bus session bus and needs a panel that supports it (KDE, XFCE, MATE,
  Cinnamon, GNOME with an AppIndicator extension, …); without a tray the client
  still runs, but quitting is only possible from the local page's button.
- Single-instance detection relies on the fixed port `127.0.0.1:9210`: if another
  program holds it, the UI falls back to a random port (the log says so) and a
  second double-click starts a second client process.
- Pausing sync disconnects from the server: while paused nothing is sent and
  nothing is received, and resuming reconnects immediately; copies made while
  paused are not replayed (same as while disconnected).
- Tray mode writes its log to `client.log` next to the config file (rotated to
  `client.log.old` past 2 MiB); console mode writes no log file and only logs to
  the terminal.
- Wayland has no compositor-wide copy events (GNOME has none at all; KDE and
  wlroots only with wl-clipboard 2.x and a data-control protocol), so Wayland
  stays poll based: detection latency ≈ the `-p` interval and identical
  re-copies are indistinguishable from "no change" (see behaviour above).
- On X11, a program that copies and immediately exits loses its clipboard data
  unless a clipboard manager takes over — event-driven monitoring only shrinks
  that window, it cannot fully avoid it.
- Image formats cover common web/office cases (PNG/JPEG/GIF/BMP/TIFF/WebP);
  exotic ones (XPM, PSD, …) are still missed.
- On Wayland, writing rich text can only hold one MIME type with `wl-copy`, so
  HTML is preferred (formatting is kept) and plain-text-only paste targets (like
  a terminal) may get nothing. X11 and Windows write both plain text and HTML
  and have no such limitation.
- Web push always targets all online clients; no per-machine targeting yet.
- Copies made while disconnected are not replayed after reconnecting — just copy
  again.
- History is browse-only; formats beyond text/image (e.g. file lists) are not
  handled.

## Roadmap

- File transfer, TLS and simple password auth
- Per-machine push, multi-select, history export/search
- Compression (lossless PNG of a screenshot can be large)

## License

[MIT](LICENSE).
