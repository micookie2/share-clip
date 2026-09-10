<p align="center">
  <img src="assets/icon.svg" width="96" height="96" alt="share-clip 图标">
</p>

# share-clip —— 局域网剪贴板共享

[![CI](https://github.com/micookie2/share-clip/actions/workflows/ci.yml/badge.svg)](https://github.com/micookie2/share-clip/actions/workflows/ci.yml)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**简体中文** · [English](README.en.md)

用 Go 实现的跨 Windows / Linux 剪贴板共享工具。client 监听本机剪贴板，把每次
复制的内容发到 server；server 实时广播给其它所有 client，并把内容存入 SQLite
历史；server 同时提供一个小型 Web 页面，可以查看历史并选择条目重新推送到
全部在线客户端。

client 有两种运行方式：**不带 `-s` 双击运行**进入桌面模式——系统托盘常驻，
同时用默认浏览器打开本地设置/日志页面；**显式给出 `-s`**（或加 `--console`）
则是原来的控制台模式，日志打在终端，适合脚本与开机自启。

支持 **文本**、**富文本（HTML）** 与 **图片**（PNG）。**文件复制暂不支持**，
会被自动忽略。富文本同步会同时携带纯文本与 HTML 两种格式：粘贴到浏览器/
Office 等富文本编辑器保留格式，粘贴到纯文本框仍得到纯文本。

```
  Windows 机器 A                Linux 机器 B
 ┌──────────────┐              ┌──────────────┐
 │ shareclip-   │  复制/推送   │ shareclip-   │
 │ client       │◄────────────►│ client       │
 │ 托盘+本地界面│              │ 托盘+本地界面│
 └──────┬───────┘   WebSocket  └──────┬───────┘
        │  ws://host:9000/ws          │
        └──────────────┬──────────────┘
                   ┌───▼────────────┐
                   │  shareclip-    │  常开机器（也可同时跑 client）
                   │  server :9000  │
                   │  + SQLite 历史 │
                   └───┬────────────┘
                       │ http://host:9000/
                ┌──────▼──────┐
                │  Web 管理页  │ 浏览器：查看历史 / 推送
                └─────────────┘
```

## 功能与行为约定

- 任一 client 复制文本或图片 → server 立即广播给**除发送者外**所有在线 client，
  并把该条写入历史（SQLite，默认保留最近 500 条，超出自动清理最旧）。
- 收到广播/Web 推送后，只把内容**写入本机剪贴板，不再回传**（防止 A↔B 两台
  机器互相广播死循环）；你本人真实的再次复制不受影响。回环抑制按“写入后本机
  剪贴板实际呈现的内容”识别，因此即使 Windows 把收到的 PNG 经 CF_DIB 位图
  往返后以不同字节重新编码，本机也不会把它当成本地新复制而回传。
- **自身副本保护**：本机刚发出的内容若被对端原样回传，或被对端降级成纯文本后
  回传，本机不会再拿它覆盖剪贴板——否则会出现“刚复制完，粘贴却变成纯文本”
  （富文本的 HTML 被回传的纯文本覆盖）。
- 同时携带文本与图片的复制以**图片优先**。Windows 与 Linux 都会先枚举剪贴板
  上实际提供的格式（Windows 的 CF_DIB、Linux 的 xclip TARGETS / wl-paste
  --list-types）：有图片格式就发图片，图片缺失或无法解码才退回文本；非 PNG
  编码（JPEG/GIF/BMP/TIFF/WebP）会在发送前转成 PNG。
- **重复内容过滤**：本次复制与**最近一条**历史内容相同时，server 直接忽略——
  不入库、也不广播。文本按“同类型 + 同字节”判定（富文本需要 HTML 与纯文本
  两者都一致才视为重复），图片按“解码后的画面相同”判定：同一张图即使被另一台
  机器（例如 Windows 的 CF_DIB 位图往返）以不同的 PNG 编码重新打包，也只会
  广播一次，不会出现“重复收到同一张图”。
  Windows 与 Linux/X11（XFixes 复制事件）都会把“相同内容再次复制”照常上报，
  Linux/Wayland 只有轮询可用、无法区分，统一放到 server 端过滤后行为一致。
  中间复制过任何不同内容后，再复制回旧内容仍会正常广播；不同机器上连续复制
  同一段内容同样只广播第一次。Web 页“推送”不写历史，不受影响。
- client 启动时不会自动推送当前剪贴板，新加入的 client 也不会自动收到历史；
  需要旧内容时在 Web 页手动推送。
- **桌面模式**：不带 `-s` 启动时进入桌面模式（系统托盘 + 本地界面），服务器
  地址等设置保存在用户配置目录，下次启动自动沿用；`--console` 或命令行给了
  `-s` 则保持控制台行为。暂停同步会断开与 server 的连接：暂停期间本机复制
  不发送、也收不到其它机器的内容，恢复后立即重连（暂停期间复制的内容不会补发）。
- **日志一行一条**：剪贴板内容、机器名等可能带换行的文本在打印前统一转义
  （`\n`、`\t` 等按字面量显示），方便直接 grep server 日志。
- **逐消息日志**：server 会打印收到的每一帧（`[recv] …`）以及发出的每次推送
  （`[push] … -> N client(s)`），client 会打印自己发出的（`[send] …`）与收到的
  （`[recv] …`）消息——hello/welcome、上下线、剪贴板广播与 Web 推送都会记录，
  文本内容附带简短预览；心跳走 WebSocket 控制帧 ping/pong，不会出现在这里；
  `-q` 可全部关掉。
- 明文传输、无鉴权，默认面向可信局域网。
- 单条内容上限默认 **32 MiB**（`-m` 可调），超限的复制会被忽略。

## 平台与依赖

| 平台 | 依赖 | 说明 |
|------|------|------|
| Windows 10/11 | 无（纯 Go syscall） | 图片经 CF_DIB 读入，自研 DIB↔PNG 转换 |
| Linux (X11) | `xclip` | 复制检测走 XFixes 事件（几乎所有现代 X server 自带），事件不可用时退回轮询；`apt install xclip` |
| Linux (Wayland) | `wl-clipboard` | 支持 `--list-types` 的 wl-clipboard 2.x 体验最佳；GNOME 等无事件通道，按 `-p` 间隔轮询；`apt install wl-clipboard` |
| 托盘（Windows / Linux） | 无（`fyne.io/systray` 纯 Go 实现） | Windows 用系统 `Shell_NotifyIcon`；Linux 走 D-Bus 会话总线上的 StatusNotifierItem，需要面板支持（KDE、XFCE、MATE、Cinnamon、GNOME + AppIndicator 扩展等） |

client 的剪贴板监听需要图形会话（`DISPLAY` 或 `WAYLAND_DISPLAY`）：控制台模式下
没有图形会话会直接报错退出，桌面模式会在界面/托盘里显示「本地环境问题」并按
退避自动重试（装好依赖后无需重启客户端）。server 无桌面要求，适合放常开机器/NAS。

托盘与本地界面不引入新的系统依赖：Windows 与 Linux 都使用纯 Go 的
`fyne.io/systray` v1.12.2（无 CGO、无 GTK，也不需要额外构建标签）。Linux 托盘
通过 D-Bus 会话总线上的 freedesktop StatusNotifierItem 协议工作，需要支持它的
桌面面板（KDE、XFCE、MATE、Cinnamon、GNOME + AppIndicator 扩展等）；检测不到
会话总线时客户端照常运行，只是没有托盘图标，退出请用本地界面上的「退出客户端」。

## 构建

需要 Go 1.25+（使用 Go 1.22 风格路由与较新的标准库）。第三方依赖：WebSocket
库 `github.com/coder/websocket`、纯 Go SQLite 驱动 `modernc.org/sqlite`、
系统托盘库 `fyne.io/systray` v1.12.2（Windows/Linux 上均为纯 Go，无 CGO/GTK，
也不需要额外构建标签）。

```bash
make                # 一次生成全部：本机版 + Windows amd64 版（bin/ 下共 4 个文件）
make build          # 仅本机：bin/shareclip-server 与 bin/shareclip-client
make build-windows  # 仅 Windows：交叉编译 bin/*.exe（Windows amd64）
# 或手动：
go build -o shareclip-server ./cmd/shareclip-server
GOOS=windows GOARCH=amd64 go build -o shareclip-client.exe ./cmd/shareclip-client
```

不想 clone 也可以直接装（二进制名即目录名）：

```bash
go install github.com/micookie2/share-clip/cmd/shareclip-server@latest
go install github.com/micookie2/share-clip/cmd/shareclip-client@latest
# 产物在 $(go env GOPATH)/bin 下
```

仓库不附带预编译二进制（`bin/` 已被 `.gitignore` 忽略），上面的构建方式在
Go 1.25.6 (linux/amd64 与 windows/amd64) 下验证通过。

## 版本号与构建信息

版本元数据放在 `internal/buildinfo`，`make build` / `make build-windows` 会在
链接期用 `-ldflags -X` 写入四样东西：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| `Version` | `git describe --tags --exact-match HEAD` | HEAD 上有 tag 就用 tag，否则用源码里的默认值 |
| `Commit` | `git rev-parse --short=7 HEAD` | 7 位短 SHA |
| `BuildTime` | `date -u +%Y-...%SZ` | 链接瞬间的 UTC 时间，日志/页面按本地时区显示 |
| `Dirty` | `git status --porcelain` | 工作区有未提交改动时版本串带 `+dirty` |

```bash
make version        # 查看本次将要注入的值
```

三个地方会用到它：

```bash
$ ./bin/shareclip-server -v
v0.1.0 (commit 0be9e27, built 2026-09-09 13:00:11 +0800, go1.25.6 linux/amd64)

$ ./bin/shareclip-server
2026-09-09 13:00:20 share-clip server v0.1.0 启动（commit 0be9e27，构建于 2026-09-09 13:00:11 +0800）
```

Web 页面顶部副标题显示 `· v0.1.0`（悬停可看 commit 与构建时间），列表底部另有一行
完整构建信息。数据来自 `GET /api/version`。

`go build` / `go install` 不经过 make，也就没有 `-ldflags`；此时 `Commit` 与构建
时间自动回退到 Go 工具链自带的 VCS 标记（提交 SHA 与提交时间），版本号仍是源码里的
默认值，因此不会显示成空白。

## 使用

```bash
# 1) 在常开机器上启动 server（默认端口 9000，数据库 shareclip.db 存于当前目录）
#    启动日志里会打印本次运行的「访问 key」，先记下来：
./shareclip-server
#    访问 key（本次启动随机生成，重启后会变）: uRRbv71iN9iVjHWHlhAp2OuSBncil6hl

# 2) 每台要共享剪贴板的机器启动 client，两种方式任选：

#    2a) 桌面模式（双击运行，或直接不带参数运行）：系统托盘常驻，
#        浏览器自动打开本地设置页（默认 http://127.0.0.1:9210），
#        第一次使用在页面里填写 server 地址与上面的访问 key 即可。
./shareclip-client

#    2b) 控制台模式（脚本、开机自启）：显式给出 server 地址与访问 key，
#        日志打在终端，不托盘、不打开页面。命令行给 -s 或加 --console 都走这条路径。
./shareclip-client -s 192.168.1.10:9000 -k uRRbv71iN9iVjHWHlhAp2OuSBncil6hl
./shareclip-client --console            # 地址与 key 取自桌面界面保存过的配置

# 3) 正常复制/粘贴即可。可选：浏览器打开 http://192.168.1.10:9000/，
#    首次会要求填写访问 key，之后查看历史、点“推送到全部在线客户端”把某条
#    内容重新推到所有机器。
```

启动参数都有单字母简写，日常用短的就够；`-h` 随时看完整说明。server 另有
`install` / `uninstall` 两个子命令（见下文「安装为 systemd 服务」）。

### server 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-a` | `--addr` | `:9000` | 监听地址，HTTP 与 WebSocket 共用同一端口 |
| `-d` | `--db` | `shareclip.db` | SQLite 历史数据库文件路径 |
| `-l` | `--history-limit` | `500` | 历史保留条数，超出自动清理最旧 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| `-k` | `--key` | 空（每次启动随机生成） | 固定访问 key；留空则启动时生成并打印到日志 |
| — | `--no-auth` | false | 关闭访问鉴权（仅限完全可信的内网，不建议） |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本号与构建信息（commit、构建时间、Go 版本/平台）后退出 |

### client 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-s` | `--server` | — | server 地址 `host:port`；不给 `-s` 时默认进桌面模式（地址在界面里设置），给了则默认进控制台模式（`-g`/`-c` 可改变默认） |
| `-k` | `--key` | — | server 的访问 key（server 启动日志里打印）；server 用 `--no-auth` 时留空 |
| `-g` | `--gui` | false | 强制桌面模式（托盘 + 本地界面），例如 `--gui -s 1.2.3.4:9000` |
| `-c` | `--console` | false | 强制控制台模式；不带 `-s`/`-k` 时使用桌面界面保存的地址与 key |
| `-n` | `--name` | 主机名 | 在其它机器/Web 页显示的机器名 |
| `-p` | `--poll` | `1000` | 监听兜底轮询间隔毫秒：X11 默认由 XFixes 复制事件驱动，事件不可用才按此间隔；Wayland（无事件通道）按此间隔 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| — | `--ui-addr` | `127.0.0.1:9210` | 桌面模式本地界面的监听地址（只监听本机回环） |
| — | `--no-open` | false | 桌面模式启动后不自动打开浏览器 |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本号与构建信息（commit、构建时间、Go 版本/平台）后退出 |

简写与长写法完全等价（`-s`、`-server`、`--server` 三种都能用，取值可写
`-s 1.2.3.4:9000` 或 `-s=1.2.3.4:9000`），因此老命令、开机自启脚本里的
`-server ...` 依旧照原样工作。`-g/--gui` 与 `-c/--console` 只是强制选择运行
方式，不改变其它选项的写法。

### 访问鉴权（key）

server **默认开启鉴权**：启动时生成一个随机 key（24 字节随机数的 base64url，
32 个字符）并打印在日志里，然后在同一进程内校验所有入口——

- **客户端连接**：WebSocket 握手必须带 `X-Shareclip-Key: <key>`（agent 自动加，
  用 `-k` 或界面里填的 key）。缺失或不对的握手直接收到 `401`，连接被拒且不会
  进入在线列表；
- **管理页与 REST API**：浏览器首次访问 `http://<server>:9000/` 会看到登录页，
  填入 key 后 server 下发一个 `HttpOnly`、`SameSite=Lax` 的**会话 Cookie**
  （令牌在内存里，不是 key 本身；进程重启即全部失效），之后页面、`/api/*` 与
  SSE 都靠它放行。也可以用 `http://<server>:9000/?key=<key>` 一次性登入——页面
  会立刻把 key 换成 Cookie 并从地址栏抹掉，免得 key 留在浏览历史里；
- 脚本/curl 也可以直接带 `X-Shareclip-Key: <key>` 或 `Authorization: Bearer <key>`。

几个常见选择：

- **不想每次重启都换 key**：用 `-k <自定义key>` 固定（例如开机自启脚本里写死）。
  注意命令行参数在本机进程列表里可见，共享机器上慎用；
- **固定 key 从哪来**：`head -c 24 /dev/urandom | base64 | tr '+/' '-_' | tr -d '='`
  之类随便生成一串即可，长度不限（最多 512 字节，且不能含换行/制表符等控制字符）；
- **完全不要鉴权**：`--no-auth`。此时任何能访问该端口的人都能连接、查看历史；
  server 启动日志里会打出警告。只应在完全可信的内网里显式使用。

key 不落盘（server 不写 key 文件），这是「每次启动随机生成」的自然结果；
client 侧填过的 key 会保存在它的配置文件里（见下文桌面模式设置）。

### 客户端桌面模式（托盘 + 本地界面）

不带 `-s` 运行（双击即可）进入桌面模式；`-g/--gui` 可强制进入，例如
`shareclip-client --gui -s 1.2.3.4:9000`（命令行给的值优先于已保存的配置）。

- **托盘 + 浏览器页面**：托盘图标常驻，同时用系统默认浏览器打开
  `http://127.0.0.1:9210`（`--ui-addr` 可改，只监听回环；`--no-open` 则不自动
  打开浏览器）。关掉页面不影响运行。
- **页面能做什么**：填写服务器地址与访问 key、本机显示名、轮询间隔与单条上限
  （保存后立即按新配置重连）；实时显示连接状态（当前阶段、server、重连倒计时）；
  实时日志（SSE 推送，可清空、可自动滚动）；页脚显示配置文件与日志文件路径、
  托盘是否可用、版本与构建信息；另有「退出客户端」按钮。
- **托盘菜单**：一行禁用状态（如 `已连接：192.168.1.10:9000`）、`打开设置与日志`、
  `启用同步`/`暂停同步` 复选框、`退出`。暂停会断开与 server 的连接：期间本机
  复制不发送、也收不到别人的内容；恢复后立即重连。
- **设置持久化**：Windows `%AppData%\share-clip\config.json`，Linux
  `~/.config/share-clip/config.json`（遵循 XDG）。字段：`server`、`key`、
  `name`、`pollMs`、`maxPayload`。桌面模式还在同目录写日志文件 `client.log`，
  超过 2 MiB 轮转为 `client.log.old`——托盘模式没有可见控制台，这个文件就是
  现场记录。
- **单实例**：再次双击时会先探测固定端口 `127.0.0.1:9210` 上是否已有 share-clip
  客户端在跑，有就只把它的页面重新打开，不再起第二个进程；该端口被别的程序占用
  时本地界面会自动退到随机端口（日志里有提示），此时再次双击会起第二个进程。
- **Windows**：客户端仍按控制台子系统编译（`-v`、在终端里运行照常有输出），
  桌面模式会隐藏双击时系统分配的控制台窗口；从已有终端启动时不会隐藏那个终端。
- **Linux**：托盘走 freedesktop **StatusNotifierItem** 协议（D-Bus 会话总线，
  由 `fyne.io/systray` 实现），需要支持它的桌面面板（KDE、XFCE、MATE、Cinnamon、
  GNOME + AppIndicator 扩展等）。检测不到会话总线时客户端照常运行，只是少了托盘
  图标（日志里会写明未检测到可用的系统托盘），退出请用页面上的「退出客户端」
  按钮。构建不需要额外系统包。
- **安全**：界面只监听回环地址，拒绝非回环 `Host`（防 DNS rebinding），所有写
  请求都要求自定义请求头 `X-Requested-With: share-clip`（防 CSRF），没有新增
  网络暴露面。
- 运行中的客户端在本地界面提供 `GET /api/health`、`/api/state`、`/api/logs`、
  `/api/events`（SSE）与 `POST /api/config`、`/api/reconnect`、`/api/pause`、
  `/api/logs/clear`、`/api/quit`。

### 安装为 systemd 服务

```bash
# 系统级（需要 root）：/etc/systemd/system/share-clip.service，开机自启
sudo install -m755 bin/shareclip-server /usr/local/bin/shareclip-server  # 先放到固定路径
sudo shareclip-server install
sudo shareclip-server install --no-start   # 只 enable，不立即启动

# 用户级（无需 root）：~/.config/systemd/user/share-clip.service
shareclip-server install --user
loginctl enable-linger <user>   # 需要登出后仍在跑 / 开机无登录会话也启动时

# 只查看会写入的单元文件与将执行的命令，不落盘、不调用 systemctl
shareclip-server install --dry-run

# 卸载：停止 + 禁用 + 删除单元 + reload；--purge 连数据目录（数据库）一起删
sudo shareclip-server uninstall
shareclip-server uninstall --user --purge
```

- `install` 写入单元文件后执行 `systemctl daemon-reload` 与
  `systemctl enable --now <单元名>`（加了 `--no-start` 就只有 `enable`）；
  用户级相关命令一律带 `--user`。
- 数据目录：系统级 `/var/lib/share-clip`（由单元的 `StateDirectory=` 创建并归属；
  默认 `DynamicUser=yes` 时实际位于 `/var/lib/private/share-clip`，
  `/var/lib/share-clip` 是指向它的符号链接），用户级 `~/.local/share/share-clip`
  （单元用 `ExecStartPre=/bin/mkdir -p` 创建）。数据库默认是数据目录下的
  `shareclip.db`。
- `install` 的 `-a/-d/-l/-m` 决定 `ExecStart` 里的监听地址、数据库路径、历史
  条数与单条上限（默认值与普通启动一致，只有数据库路径默认落在数据目录而不是
  当前目录）；**服务器的其它参数**（当前是 `-q`，以及以后新增的参数）用可重复的
  `--exec-arg` 追加，例如 `sudo shareclip-server install --exec-arg=-q
  --exec-arg=-l --exec-arg=1000`，等价的单元行是
  `ExecStart=…/shareclip-server -a :9000 -d … -l 500 -m 33554432 -q -l 1000`
  （重复的标量参数后者生效，所以这样也能覆盖前面的默认值）。
- **鉴权**：默认开启，key 每次启动随机生成并写进 journal——
  `journalctl -u share-clip -f | grep 访问 key` 就能看到（用户级加 `--user`）。
  想固定 key（脚本、开机自启方便）用 `--exec-arg=-k --exec-arg=<你的key>`；
  想彻底关闭用 `--no-auth`。注意 key 写在 `ExecStart` 里时单元文件是 0644，
  同机其他用户可见，共享机器上别这么做。
- 想改的不只是启动参数（环境变量、资源限制、依赖关系等）时，用 systemd 原生的
  覆盖，不必改单元文件本身：`sudo systemctl edit share-clip` 会生成
  `/etc/systemd/system/share-clip.service.d/override.conf`，在里面写
  `[Service]`；要换掉 `ExecStart` 必须先写一行空的 `ExecStart=` 清空原值，再写
  新的 `ExecStart=…`（`Type=simple` 不允许两条 `ExecStart`）。drop-in 与主单元
  分开存放，因此重新 `install --force` 不会覆盖它。
- 单元固定带 `Restart=always`、`RestartSec=2`、journald 日志、
  `After/Wants=network-online.target`，以及加固项（`NoNewPrivileges`、
  `PrivateTmp`、`ProtectSystem=full`；系统级另有 `ProtectHome=true`）。
- 排障：`systemctl status share-clip`、`journalctl -u share-clip -f`；用户级加
  `--user`（`systemctl --user status share-clip`、`journalctl --user -u share-clip -f`）。

**务必先安装到固定路径**：`install` 用的是**当前正在运行的那个可执行文件**的
路径。`go run` 的临时产物会被直接拒绝（这类文件重启后就不存在了）；家目录下的
二进制（如 `~/go/bin/shareclip-server`）在系统级单元里也不可用，因为系统级单元
带 `ProtectHome=true`，家目录对它不可见。正确做法是先 `go build`，再
`sudo install -m755 bin/shareclip-server /usr/local/bin/`，然后用该路径运行
`install`。

#### install 选项

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-a` | `--addr` | `:9000` | 写进 `ExecStart` 的监听地址 |
| `-d` | `--db` | 数据目录下的 `shareclip.db` | SQLite 数据库路径；留空按作用域推导 |
| `-l` | `--history-limit` | `500` | 历史保留条数 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| `-u` | `--user` | false | 安装为用户级服务（`~/.config/systemd/user`，无需 root） |
| `-n` | `--name` | `share-clip` | 单元名（不含 `.service`） |
| — | `--run-as` | 空（`DynamicUser=yes`） | 系统级服务以该用户运行；用户级会忽略此选项 |
| — | `--no-auth` | false | 写进 `ExecStart`：关闭 server 的访问鉴权 |
| — | `--exec-arg` | 空 | 追加到 `ExecStart` 的额外启动参数，可重复（如 `--exec-arg=-q`）；服务器将来新增的参数也走这里，后出现的标量参数会覆盖前面的 |
| — | `--unit-dir` | 按作用域 | 覆盖单元文件目录 |
| — | `--no-start` | false | 只 enable，不立即启动 |
| `-f` | `--force` | false | 覆盖已存在的单元文件 |
| — | `--dry-run` | false | 只打印单元文件与将执行的命令 |

#### uninstall 选项

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-u` | `--user` | false | 卸载用户级服务（`~/.config/systemd/user`） |
| `-n` | `--name` | `share-clip` | 单元名（不含 `.service`） |
| — | `--unit-dir` | 按作用域 | 覆盖单元文件目录 |
| — | `--purge` | false | 同时删除数据目录（数据库） |
| — | `--dry-run` | false | 只打印将删除的文件与将执行的命令 |

## Web 页面（server 管理页）

`http://<server>:9000/`（默认监听全部网卡，内网可访问）。**首次打开会要求填写
访问 key**（server 启动日志里打印的那串；也可以用
`http://<server>:9000/?key=<key>` 一次性登入）。登录后 server 下发一个会话
Cookie，页面与它的所有接口都靠它放行（页面上的「退出」按钮会注销会话并回到
登录页，共享电脑上离开时用）。之后的功能有：

- **实时自动刷新**：页面通过 Server-Sent Events（`/api/events`）与服务端保持
  长连接——任何客户端复制新内容都会即时出现在列表顶部（新记录以短暂高亮标出），
  节点上下线即时更新，断线自动重连并补齐期间错过的记录，全程无需手动刷新；
- **顶部实时节点数**：标题栏常驻显示当前在线接入的节点数量（在线时指示灯为绿色），
  点击可查看在线节点名单；浏览器标签页标题也会同步显示 `(N 台在线)`；
- 历史列表（最新在前，每次 50 条 + “加载更多”）：文本可展开全文/复制，图片
  缩略图点击放大（灯箱预览）；时间统一显示精确时间 `YYYY-MM-DD HH:mm:ss`
  （浏览器本地时区，悬停可见服务端原始时间戳）；
- 每条记录有“推送到全部在线客户端”按钮（向**所有**当前在线 client 写入该
  内容，与来源无关），操作结果以 toast 提示；
- “清空”按钮清空数据库，其它已打开的页面实时同步清空；
- 顶部与底部展示当前 server 的**版本号与构建时间**（编译期注入，见「版本号与构建信息」）；
- 界面为扁平简洁风：白底卡片 + 1px 细描边，纯色强调（无渐变、无浮起阴影），
  自适应亮色/暗色主题（跟随系统），并适配手机屏幕。

相关接口：`GET /api/status`（在线节点快照 JSON）、`GET /api/version`（版本号、
commit、构建时间、Go 版本/平台）、`GET /api/events`（SSE 事件流：`status` /
`clip` / `cleared`），以及登录用的 `POST /api/login` 与 `POST /api/logout`。
除 `GET /login`、`POST /api/login`、`POST /api/logout` 外，所有端点都要求
访问 key 或会话 Cookie（见「访问鉴权（key）」）。

## 代码结构

```
assets/                 品牌图标（icon.svg 为源，PNG/ICO 与 Windows .syso 由它生成）
cmd/shareclip-server/   server 入口（含 install / uninstall 子命令）
cmd/shareclip-client/   client 入口：选择控制台/桌面模式，串联托盘、本地界面与 agent
internal/agent/         client 主逻辑：监听剪切板、收发、自动重连（含连接状态回调）
internal/appconfig/     client 持久化设置：config.json 路径、默认值、读写、地址规范化
internal/auth/          访问 key 生成/校验（恒定时间比较）+ 管理页会话令牌（内存）
internal/clipboard/     跨平台剪贴板抽象 + Watcher（防回环/去重语义）
  clipboard_windows.go  Win32：CF_UNICODETEXT / CF_HTML / CF_DIB（stdlib syscall）
  clipboard_linux.go    Linux：xclip / wl-clipboard，枚举格式+图优先+富文本+多格式转 PNG
  x11owner_linux.go     X11：原生 CLIPBOARD 选择所有者（同时提供 text/plain 与 text/html）
  x11watch_linux.go     X11：XFixes 复制事件监听（事件驱动，不可用时退回轮询）
internal/buildinfo/     版本号/commit/构建时间（-ldflags 注入，日志、-v、Web 共用）
internal/dib/           纯 Go DIB↔PNG 编解码（Windows 图片用）
internal/cli/           命令行选项：长写法 + 单字母简写共用一个变量
internal/clientsvc/     client 服务：配置 + agent 生命周期 + 状态快照 + 日志环形缓冲
internal/clientui/      桌面模式本地界面：只监听回环的 HTTP 服务（防 DNS rebinding/CSRF）
  webui/index.html      设置/状态/日志页面（内嵌，浏览器打开）
internal/logx/          日志出口：保证一条事件只占一行（托盘模式另写 client.log）
internal/protocol/      消息格式（JSON 头 + 二进制负载）
internal/server/        WS 集线器、广播、SSE 事件、Web API 与内嵌页面
internal/store/         SQLite 历史存储（清理/分页/内容读取）
internal/systemd/       systemd 单元生成 + install/uninstall（纯函数 + 可替换执行器）
internal/tray/          系统托盘图标与菜单（fyne.io/systray）
```

## 测试

```bash
go test -count=1 ./...   # 协议/DIB/Watcher/存储/客户端服务/托盘/systemd/e2e 广播与推送
```

e2e 测试用真实的 WebSocket 连接验证：广播给除发送者外所有 client、图片内容、
历史入库、Web 内容读取与 Web 推送、清空历史。托盘菜单模型、systemd 单元生成与
安装流程（替换执行器，不真的调用 systemctl）、客户端配置与服务生命周期都有
各自的单元测试，不需要图形会话或 systemd。

## 已知限制（有意为之，v1 范围）

- 明文、无鉴权：仅适合可信局域网；跨公网请自行加 VPN/TLS。
- 桌面模式的界面是浏览器页面，不是原生窗口：看日志/改设置都得开着那个页面
  （关掉页面不影响同步）。
- 桌面模式的 Linux 托盘依赖 D-Bus 会话总线上的 StatusNotifierItem 协议，需要
  支持它的桌面面板（KDE、XFCE、MATE、Cinnamon、GNOME + AppIndicator 扩展等）；
  没有托盘时客户端仍会运行，只是退出只能靠本地页面上的按钮。
- 单实例靠固定端口 `127.0.0.1:9210` 探测：该端口被别的程序占用时本地界面会
  退到随机端口（日志有提示），此时再次双击会启动第二个客户端进程。
- 暂停同步会断开与 server 的连接：暂停期间既不发送本机复制，也收不到其它机器
  的内容，恢复后立即重连；暂停期间复制的内容不会补发（与断线期间一致）。
- 托盘模式的日志写在配置文件旁的 `client.log`（超过 2 MiB 轮转为
  `client.log.old`）；控制台模式不写日志文件，日志只在终端。
- Wayland 无合成器级复制事件（GNOME 完全没有，KDE/wlroots 也只在装了
  wl-clipboard 2.x 且有 data-control 协议时才支持），因此 Wayland 侧仍是轮询：
  检测延迟 ≈ `-p` 间隔，“相同内容再次复制”无法识别（见上文行为约定）。
- X11 下若复制方程序“复制后立即退出”且桌面没有剪贴板管理器接管，内容会随
  程序退出而消失——事件驱动也只能把漏检窗口缩到最小，无法完全避免。
- 图片格式覆盖常见 Web/办公场景（PNG/JPEG/GIF/BMP/TIFF/WebP），小众格式
  （XPM、PSD 等）仍会漏。
- Wayland 侧写富文本时，`wl-copy` 一次只能持有一种 MIME，因此会优先写入
  HTML（保格式）；粘贴到只接受纯文本的目标（如终端）时可能取不到纯文本。
  X11 与 Windows 会同时写入纯文本与 HTML，无此限制。
- Web 推送只能推送到全部在线 client，暂不支持指定某台机器。
- 断线期间本机复制的内容不会补发（重连后重新复制即可）。
- 历史仅展示；文件列表等文本/图片之外的格式不处理。

## Roadmap（可能的方向）

- 文件传输、TLS 与简单密码鉴权
- Web 推送支持指定机器、批量选择、历史导出/搜索
- 压缩传输（图片 PNG 无损体积可能偏大）

## 许可

[MIT](LICENSE)。英文说明见 [README.en.md](README.en.md)。
