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
Office 等富文本编辑器保留格式，粘贴到纯文本框仍得到纯文本。Windows 侧按微软
[HTML Clipboard Format](https://learn.microsoft.com/en-us/windows/win32/dataxchg/html-clipboard-format)
规范处理 CF_HTML：跨机传输的是**完整 HTML 文档**（连同 `<style>` 里的类、
`<meta name=ProgId content=Excel.Sheet>` 等格式上下文），而不只是
`<!--StartFragment-->` 片段，所以从 Excel/Word 复制、到对端再粘贴回
Excel/Word 仍能保留格式（Linux 侧对应 `text/html` 目标）。局限：Excel 的原生
剪贴板格式（Biff/XML Spreadsheet、透视表等）不跨机传输，个别 Excel 专有细节
（例如列宽）仍可能不同——Excel 自己粘贴 HTML 时也会忽略列宽；不消费 HTML 的
程序只能拿到纯文本。新旧版本客户端可以互通，但只有**两端都升级**到本版本才能
完整保留格式（旧版本跨机传的是片段，粘贴时格式可能降级）。

```
  Windows 机器 A                Linux 机器 B
 ┌──────────────┐              ┌──────────────┐
 │ shareclip-   │  复制/推送   │ shareclip-   │
 │ client       │◄────────────►│ client       │
 │ 托盘+本地界面│              │ 托盘+本地界面│
 └──────┬───────┘   WebSocket  └──────┬───────┘
        │  ws://host:9090/ws          │
        └──────────────┬──────────────┘
                   ┌───▼────────────┐
                   │  shareclip-    │  常开机器（也可同时跑 client）
                   │  server :9090  │
                   │  + SQLite 历史 │
                   └───┬────────────┘
                       │ http://host:9090/
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
  （富文本的 HTML 被回传的纯文本覆盖）。图片回传按**解码后的画面**识别（而非
  只比对字节），因此同一张图被对端重新编码后回传也不会覆盖本机剪贴板。
- **不拿纯文本覆盖本机同内容的富文本**：收到的内容若只是纯文本（没有 HTML）、
  而本机剪贴板上已经有**同一段文字且带格式**的版本，就保留本机的富文本不写入
  （日志会记一条“对端只发来纯文本版本…保留本机格式”）。对端的格式可能是在半路
  丢掉的（例如虚拟机/云桌面剪贴板代理只同步纯文本），若照写就会把本机好好的
  格式覆盖成纯文本。内容不同、或对端自带 HTML 时仍照常写入。
- 同时携带文本与图片的复制以**文本优先**：只要剪贴板上的文本可用（非空、不是
  文件 URI 列表）就同步文本（有 `text/html` 时一并携带 HTML）；只有没有可用
  文本（截图、图片编辑器等）时才取图片，非 PNG 编码（JPEG/GIF/BMP/TIFF/WebP）
  发送前转成 PNG（该转换针对 Linux 提供的图片格式；Windows 只读 CF_DIB，并统一
  重新编码成 PNG）。Windows 与 Linux 都先枚举剪贴板上实际提供的格式（Windows 的
  CF_DIB、Linux 的 xclip TARGETS / wl-paste --list-types），文本胜出时图片
  根本不会被读取，因此也不会迫使 Office 程序为一份用不上的图片做延迟渲染。
  之所以这样改：Excel/LibreOffice/WPS 复制单元格时会在真实文本旁边附一份
  渲染好的图片，图片优先会让“复制文字”到对端变成“复制图片”。代价是极少数
  同时提供文本与图片的“图片复制”现在会按文本同步（粘贴得到的仍是文本）。
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
# 或手动（Windows 客户端要带 -H=windowsgui 才是 GUI 子系统，双击不会开终端）：
go build -o shareclip-server ./cmd/shareclip-server
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" -o shareclip-client.exe ./cmd/shareclip-client
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
# 1) 在常开机器上启动 server（默认端口 9090，数据库 shareclip.db 存于当前目录）
#    启动日志里会打印本次运行的「访问 key」，先记下来：
./shareclip-server
#    访问 key（本次启动随机生成，重启后会变）: uRRbv71iN9iVjHWHlhAp2OuSBncil6hl
#    想装成开机自启的 systemd 服务（交互式问答，默认装到 /opt/share-clip/）：
sudo ./shareclip-server install

# 2) 每台要共享剪贴板的机器启动 client，两种方式任选：

#    2a) 桌面模式（双击运行，或直接不带参数运行）：系统托盘常驻，
#        浏览器自动打开本地设置页（默认 http://127.0.0.1:9210），
#        第一次使用在页面里填写 server 地址与上面的访问 key 即可。
./shareclip-client

#    2b) 控制台模式（脚本、开机自启）：显式给出 server 地址与访问 key，
#        日志打在终端，不托盘、不打开页面。命令行给 -s 或加 --console 都走这条路径。
./shareclip-client -s 192.168.1.10:9090 -k uRRbv71iN9iVjHWHlhAp2OuSBncil6hl
./shareclip-client --console            # 地址与 key 取自桌面界面保存过的配置

# 3) 正常复制/粘贴即可。可选：浏览器打开 http://192.168.1.10:9090/，
#    首次会要求填写访问 key，之后查看历史、点“推送到全部在线客户端”把某条
#    内容重新推到所有机器。
```

启动参数都有单字母简写，日常用短的就够；`-h` 随时看完整说明。server 另有
`install` / `uninstall` 两个子命令（见下文「安装为 systemd 服务」）。

### server 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-a` | `--addr` | `:9090` | 监听地址，HTTP 与 WebSocket 共用同一端口 |
| `-d` | `--db` | `shareclip.db` | SQLite 历史数据库文件路径 |
| `-l` | `--history-limit` | `500` | 历史保留条数，超出自动清理最旧 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| `-k` | `--key` | 空 | 固定访问 key；优先级低于显式 `-K`、高于环境变量与随机 |
| `-K` | `--key-file` | 空 | 从文件读取访问 key（`install` 生成的 `shareclip.key` 即此格式：`SHARECLIP_KEY=<key>`） |
| — | `--no-auth` | false | 关闭访问鉴权（仅限完全可信的内网，不建议） |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本号与构建信息（commit、构建时间、Go 版本/平台）后退出 |

key 的取值顺序：`-k` > `-K` 文件 > 环境变量 `SHARECLIP_KEY`（systemd
单元经 `EnvironmentFile=` 注入的就是它）> 每次启动随机生成并打印到日志。

### 访问 key 的持久化（key 子命令）

`install` 时自动生成一个固定 key，写入**与可执行文件同目录**的
`shareclip.key`（默认即 `/opt/share-clip/shareclip.key`，权限 0600），
此后每次启动——包括重启、升级重装——都一直用这一个 key。key 不写进单元
文件正文：单元只有一行 `EnvironmentFile=/opt/share-clip/shareclip.key`，
由 systemd 以 root 读取注入服务进程，journal、`systemctl cat`、`ps` 里
都看不到 key 值。

```bash
sudo shareclip-server key                # 显示当前 key 与文件位置
sudo shareclip-server key -r             # 生成新的随机 key（写入文件并自动重启服务）
sudo shareclip-server key --set <key>    # 设置为指定 key
shareclip-server key --user              # 按用户级服务定位
```

更换 key 后所有客户端与管理页都要改用新 key。`key` 会先从已安装单元的
`EnvironmentFile=` 定位文件，找不到时退回当前程序同目录的 `shareclip.key`。
`uninstall --purge` 会把 key 文件一并删除（只按文件名 `shareclip.key` 认，
绝不误删他人单元里的 EnvironmentFile）。

### client 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-s` | `--server` | — | server 地址 `host:port`；不给 `-s` 时默认进桌面模式（地址在界面里设置），给了则默认进控制台模式（`-g`/`-c` 可改变默认） |
| `-k` | `--key` | — | server 的访问 key（server 启动日志里打印）；server 用 `--no-auth` 时留空 |
| `-g` | `--gui` | false | 强制桌面模式（托盘 + 本地界面），例如 `--gui -s 1.2.3.4:9090` |
| `-c` | `--console` | false | 强制控制台模式；不带 `-s`/`-k` 时使用桌面界面保存的地址与 key |
| `-n` | `--name` | 主机名 | 在其它机器/Web 页显示的机器名 |
| `-p` | `--poll` | `1000` | 监听兜底轮询间隔毫秒：X11 默认由 XFixes 复制事件驱动，事件不可用才按此间隔；Wayland（无事件通道）按此间隔 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| — | `--ui-addr` | `127.0.0.1:9210` | 桌面模式本地界面的监听地址（只监听本机回环） |
| — | `--no-open` | false | 桌面模式启动后不自动打开浏览器 |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本号与构建信息（commit、构建时间、Go 版本/平台）后退出 |

简写与长写法完全等价（`-s`、`-server`、`--server` 三种都能用，取值可写
`-s 1.2.3.4:9090` 或 `-s=1.2.3.4:9090`），因此老命令、开机自启脚本里的
`-server ...` 依旧照原样工作。`-g/--gui` 与 `-c/--console` 只是强制选择运行
方式，不改变其它选项的写法。

### 访问鉴权（key）

server **默认开启鉴权**：启动时生成一个随机 key（24 字节随机数的 base64url，
32 个字符）并打印在日志里，然后在同一进程内校验所有入口——

- **客户端连接**：WebSocket 握手必须带 `X-Shareclip-Key: <key>`（agent 自动加，
  用 `-k` 或界面里填的 key）。缺失或不对的握手直接收到 `401`，连接被拒且不会
  进入在线列表；
- **管理页与 REST API**：浏览器首次访问 `http://<server>:9090/` 会看到登录页，
  填入 key 后 server 下发一个 `HttpOnly`、`SameSite=Lax` 的**会话 Cookie**
  （令牌在内存里，不是 key 本身；进程重启即全部失效），之后页面、`/api/*` 与
  SSE 都靠它放行。也可以用 `http://<server>:9090/?key=<key>` 一次性登入——页面
  会立刻把 key 换成 Cookie 并从地址栏抹掉，免得 key 留在浏览历史里；
- 脚本/curl 也可以直接带 `X-Shareclip-Key: <key>` 或 `Authorization: Bearer <key>`。

几个常见选择：

- **不想每次重启都换 key**：用 `install` 部署 systemd 服务（自动生成并固定使用
  `shareclip.key`，见「访问 key 的持久化」），或临时用 `-k <key>` /
  `-K <文件>` / 环境变量 `SHARECLIP_KEY` 固定。注意 `-k` 的命令行参数在本机
  进程列表里可见，共享机器上慎用；`-K` 文件方式没有这个问题；
- **固定 key 从哪来**：`head -c 24 /dev/urandom | base64 | tr '+/' '-_' | tr -d '='`
  之类随便生成一串即可，长度不限（最多 512 字节，且不能含换行/制表符等控制字符）；
- **完全不要鉴权**：`--no-auth`。此时任何能访问该端口的人都能连接、查看历史；
  server 启动日志里会打出警告。只应在完全可信的内网里显式使用。

直接运行时 key 不落盘（server 进程自己从不主动写 key 文件），这是「每次启动
随机生成」的自然结果；`install` 部署时生成的 `shareclip.key` 除外（见上文
「访问 key 的持久化」）。client 侧填过的 key 会保存在它的配置文件里
（见下文桌面模式设置）。

### 客户端桌面模式（托盘 + 本地界面）

不带 `-s` 运行（双击即可）进入桌面模式；`-g/--gui` 可强制进入，例如
`shareclip-client --gui -s 1.2.3.4:9090`（命令行给的值优先于已保存的配置）。

- **托盘 + 浏览器页面**：托盘图标常驻，同时用系统默认浏览器打开
  `http://127.0.0.1:9210`（`--ui-addr` 可改，只监听回环；`--no-open` 则不自动
  打开浏览器）。关掉页面不影响运行。
- **页面能做什么**：填写服务器地址与访问 key、本机显示名、轮询间隔与单条上限
  （保存后立即按新配置重连）；实时显示连接状态（当前阶段、server、重连倒计时）；
  实时日志（SSE 推送，可清空、可自动滚动）；页脚显示配置文件与日志文件路径、
  托盘是否可用、版本与构建信息；另有「退出客户端」按钮。
- **托盘菜单**：一行禁用状态（如 `已连接：192.168.1.10:9090`）、`打开设置与日志`、
  `启用同步`/`暂停同步` 复选框、`退出`。暂停会断开与 server 的连接：期间本机
  复制不发送、也收不到别人的内容；恢复后立即重连。
- **设置持久化**：Windows `%AppData%\share-clip\config.json`，Linux
  `~/.config/share-clip/config.json`（遵循 XDG）。字段：`server`、`key`、
  `name`、`pollMs`、`maxPayload`。桌面模式还在同目录写日志文件 `client.log`，
  超过 2 MiB 轮转为 `client.log.old`——托盘模式没有可见控制台，这个文件就是
  现场记录。
- **单实例**：客户端现在强制单实例（同一用户、同一台机器只跑一个）：再次启动
  （包括 `--console`）会以明确提示退出；桌面模式双击时若已有实例在运行，则只把
  它的本地页面重新打开，不再起第二个进程。本地页面仍默认监听 `127.0.0.1:9210`，
  该端口被别的程序占用时会退到随机端口（日志里有提示）。
- **Windows**：客户端按 **GUI 子系统**链接（`make build-windows` 会加
  `-H=windowsgui`），双击运行时系统根本不会给它分配控制台，托盘图标背后没有任何
  终端窗口（控制台子系统的老做法是「先建窗口再隐藏」，在把 Windows Terminal 设为
  默认终端的 Windows 11 上藏不干净，会留下一个可见页签）。需要输出时客户端会自己
  附着到父终端的控制台：从 cmd/PowerShell 里跑 `-v`、`-s ...`、`--console` 照常有
  输出；真没有终端却又要求控制台模式时，它才自己开一个。绕过 Makefile 直接
  `go build`/`go install` 得到的是控制台子系统版本，功能一样（双击时退回隐藏黑窗口，
  在 Windows 11 默认终端下可能留下终端窗口），要干净的托盘体验请用 `make build-windows`。
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

`install` 默认是**交互式安装**：逐项询问安装位置、监听端口、数据目录、历史
条数、鉴权方式、运行用户、单元名与启动策略，每一项都给出默认值（`[方括号]`），
直接回车即采用；命令行上显式给过的选项不再重复提问；最后打印配置摘要并让你
确认才落盘。标准输入不是终端（管道、CI）时自动跳过全部提问，效果等同 `--yes`。

```bash
# 系统级（需要 root）：可执行文件安装到 /opt/share-clip/，
# 单元写入 /etc/systemd/system/share-clip.service，开机自启
sudo shareclip-server install              # 交互式
sudo shareclip-server install --yes        # 不提问：/opt/share-clip + :9090 + 默认值
sudo shareclip-server install --no-start   # 只 enable，不立即启动

# 用户级（无需 root）：可执行文件默认装到 ~/.local/bin，
# 单元写入 ~/.config/systemd/user/share-clip.service
shareclip-server install --user
loginctl enable-linger <user>   # 需要登出后仍在跑 / 开机无登录会话也启动时

# 只查看会写入的单元文件与将执行的命令，不落盘、不调用 systemctl
shareclip-server install --dry-run

# 卸载：停止 + 禁用 + 删除单元 + reload；--purge 连数据目录、安装的
# 可执行文件、数据库与 key 文件一起删
sudo shareclip-server uninstall
shareclip-server uninstall --user --purge
```

- 安装会把**当前正在运行的可执行文件复制**到安装位置（交互提问，默认系统级
  `/opt/share-clip/`、用户级 `~/.local/bin/`），单元里的 `ExecStart` 指向这份
  副本；日后换构建产物重新跑一次 `install` 即完成升级，`go run` 的临时产物也
  可以直接装（服务用的是复制件，不依赖临时文件存活）。
- `install` 写入单元文件后执行 `systemctl daemon-reload` 与
  `systemctl enable --now <单元名>`（加了 `--no-start` 就只有 `enable`）；
  用户级相关命令一律带 `--user`。
- 数据目录（交互里问的是目录，数据库文件固定为其中的 `shareclip.db`）：
  系统级默认 `/var/lib/share-clip`（由单元的 `StateDirectory=` 创建并归属；
  默认 `DynamicUser=yes` 时实际位于 `/var/lib/private/share-clip`，
  `/var/lib/share-clip` 是指向它的符号链接），用户级默认
  `~/.local/share/share-clip`。选了自定义目录时安装器会提前建好并处理归属：
  配 `--run-as` 就 chown 给该用户（推荐）；用动态用户则只能放宽到 `0777`
  并打印警告。
- `install` 的 `-a/-d/-l/-m` 决定 `ExecStart` 里的监听地址、数据库路径、历史
  条数与单条上限（默认值与普通启动一致，只有数据库路径默认落在数据目录而不是
  当前目录）；**服务器的其它参数**（当前是 `-q`，以及以后新增的参数）用可重复的
  `--exec-arg` 追加，例如 `sudo shareclip-server install --exec-arg=-q
  --exec-arg=-l --exec-arg=1000`，等价的单元行是
  `ExecStart=…/shareclip-server -a :9090 -d … -l 500 -m 33554432 -q -l 1000`
  （重复的标量参数后者生效，所以这样也能覆盖前面的默认值）。
- **鉴权**：默认开启，且 key 是**安装时生成的固定值**——保存到与可执行文件
  同目录的 `shareclip.key`（默认 `/opt/share-clip/shareclip.key`，权限 0600），
  重启、升级重装都一直用这一个。单元只写一行 `EnvironmentFile=…/shareclip.key`，
  由 systemd 以 root 读取注入环境，journal、`systemctl cat`、`ps` 里都看不到
  key 值。查看用 `sudo shareclip-server key`，更换用
  `sudo shareclip-server key -r`（自动重启服务）或 `--set <key>`。
  交互安装里也可留空自动生成或直接输入一个；`--no-auth` 彻底关闭鉴权。
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

**升级与安装来源**：交互问答的「安装位置」决定可执行文件复制到哪（默认
`/opt/share-clip/`）；直接 `go build -o /opt/share-clip/shareclip-server` 后
原地运行 `install` 也支持（检测到源与目标同一文件时自动跳过复制）。

#### install 选项

交互模式下列出的默认值就是各问题的默认答案；命令行显式给出的选项不再提问。

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-y` | `--yes` | false | 非交互：不提问，直接用命令行参数与各项默认值 |
| — | `--dir` | 系统级 `/opt/share-clip`，用户级 `~/.local/bin` | 安装位置（可执行文件复制到的目录） |
| `-a` | `--addr` | `:9090` | 写进 `ExecStart` 的监听地址；交互里只填端口也可 |
| `-d` | `--db` | 数据目录下的 `shareclip.db` | SQLite 数据库完整路径；留空按作用域推导 |
| `-l` | `--history-limit` | `500` | 历史保留条数 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数（交互里按 MB 问） |
| `-k` | `--key` | 空（自动生成） | 指定访问 key 并写入安装目录的 `shareclip.key`（覆盖已有）；留空=自动生成/沿用现有文件 |
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
| — | `--purge` | false | 同时删除数据目录、安装的可执行文件、数据库（含 `-wal`/`-shm`）与 key 文件 |
| — | `--dry-run` | false | 只打印将删除的文件与将执行的命令 |

## Web 页面（server 管理页）

`http://<server>:9090/`（默认监听全部网卡，内网可访问）。**首次打开会要求填写
访问 key**（直接运行看 server 启动日志，systemd 部署用 `shareclip-server key`
查看；也可以用
`http://<server>:9090/?key=<key>` 一次性登入）。登录后 server 下发一个会话
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
  clipboard_linux.go    Linux：xclip / wl-clipboard，枚举格式+文本优先+富文本+图片兜底+多格式转 PNG
  x11owner_linux.go     X11：原生 CLIPBOARD 选择所有者（ICCCM 协议 target + text/plain 与 text/html）
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
- 单实例由客户端自己强制（同一用户只允许一个实例，锁文件在该用户的配置目录里）：
  第二个实例会以明确提示退出，桌面模式双击只会打开已有实例的本地界面；本地界面
  端口 `127.0.0.1:9210` 被别的程序占用时仍会退到随机端口（日志有提示）。不同用户
  （或给第二个实例换了 `XDG_CONFIG_HOME`/配置目录）不在同一把锁的范围内。
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
- 富文本跨机同步的是标准 HTML：Excel 的原生剪贴板格式（Biff/XML Spreadsheet、
  透视表、列宽等）不传输，个别 Excel 专有格式细节仍可能不同（列宽本来也会被
  Excel 的 HTML 粘贴忽略）；不消费 HTML 的程序只能拿到纯文本。
- **云桌面/虚拟机剪贴板代理会让本机富文本粘贴丢格式**：spice-vdagent（SPICE
  云桌面）、VMware Tools、VirtualBox 增强功能等只同步纯文本，会在本机复制后
  约 150ms 内接管剪贴板、丢掉 HTML，所以**本机**粘贴只剩纯文本；**对端不受
  影响**——share-clip 检测到这种降级会忽略纯文本版本（日志里会出现“本机剪贴板
  被其它程序改写为纯文本…已忽略”），继续保留对端的富文本。要恢复本机富文本，
  需要停用该代理的剪贴板共享（例如停掉 `spice-vdagent` 与 `vdagent-monitor.sh`，
  代价是宿主机↔虚拟机之间不能再复制粘贴）；复制后立即粘贴也能赶在代理接管前
  拿到富文本。
- Web 推送只能推送到全部在线 client，暂不支持指定某台机器。
- 断线期间本机复制的内容不会补发（重连后重新复制即可）。
- **client 的 `-m/--max-payload` 不要超过 server 的 `-m`**：client 侧新加的
  “超限跳过”只按本机上限判断，帧一旦超过 **server** 的上限，server 会直接关闭
  连接（WebSocket 1009），这条内容不会被同步；两边保持一致最稳妥。
- 历史仅展示；文件列表等文本/图片之外的格式不处理。

## Roadmap（可能的方向）

- 文件传输、TLS 与简单密码鉴权
- Web 推送支持指定机器、批量选择、历史导出/搜索
- 压缩传输（图片 PNG 无损体积可能偏大）

## 许可

[MIT](LICENSE)。英文说明见 [README.en.md](README.en.md)。
