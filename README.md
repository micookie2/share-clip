# share-clip —— 局域网剪贴板共享

[![CI](https://github.com/micookie2/share-clip/actions/workflows/ci.yml/badge.svg)](https://github.com/micookie2/share-clip/actions/workflows/ci.yml)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**简体中文** · [English](README.en.md)

用 Go 实现的跨 Windows / Linux 剪贴板共享工具。client 监听本机剪贴板，把每次
复制的内容发到 server；server 实时广播给其它所有 client，并把内容存入 SQLite
历史；server 同时提供一个小型 Web 页面，可以查看历史并选择条目重新推送到
全部在线客户端。

支持 **文本** 与 **图片**（PNG）。**文件复制暂不支持**，会被自动忽略。

```
  Windows 机器 A                Linux 机器 B
 ┌──────────────┐              ┌──────────────┐
 │ shareclip-   │  复制/推送    │ shareclip-   │
 │ client       │◄────────────►│ client       │
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
  机器互相广播死循环）；你本人真实的再次复制不受影响。
- 同时携带文本与图片的复制以**图片优先**（Windows 可直接枚举剪贴板格式；
  X11 下按“文本为空才尝试图片”的启发式，个别场景可能漏发图片）。
- **重复内容过滤**：本次复制与**最近一条**历史内容完全相同（同类型 + 同字节）
  时，server 直接忽略——不入库、也不广播。Windows 上系统剪贴板序列号会让
  “相同内容再次复制”照常上报、Linux 的 xclip/wl-paste 轮询（检测延迟约
  0.5–1 秒，`-p` 可调）则无法识别，统一放到 server 端过滤后两个平台行为
  一致。中间复制过任何不同内容后，再复制回旧内容仍会正常广播；不同机器上
  连续复制同一段内容同样只广播第一次。Web 页“推送”不写历史，不受影响。
- client 启动时不会自动推送当前剪贴板，新加入的 client 也不会自动收到历史；
  需要旧内容时在 Web 页手动推送。
- 明文传输、无鉴权，默认面向可信局域网。
- 单条内容上限默认 **32 MiB**（`-m` 可调），超限的复制会被忽略。

## 平台与依赖

| 平台 | 依赖 | 说明 |
|------|------|------|
| Windows 10/11 | 无（纯 Go syscall） | 图片经 CF_DIB 读入，自研 DIB↔PNG 转换 |
| Linux (X11) | `xclip` | `apt install xclip` / `dnf install xclip` |
| Linux (Wayland) | `wl-clipboard` | `apt install wl-clipboard` 等 |

client 需要运行在有图形会话（`DISPLAY` 或 `WAYLAND_DISPLAY`）的桌面环境中；
无图形会话会直接报错退出。server 无桌面要求，适合放常开机器/NAS。

## 构建

需要 Go 1.25+（使用 Go 1.22 风格路由与较新的标准库）。第三方依赖：WebSocket
库 `github.com/coder/websocket`、纯 Go SQLite 驱动 `modernc.org/sqlite`。

```bash
make build          # 生成 bin/shareclip-server 与 bin/shareclip-client（Linux）
make build-windows  # 交叉编译 bin/*.exe（Windows amd64）
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

## 使用

```bash
# 1) 在常开机器上启动 server（默认端口 9000，数据库 shareclip.db 存于当前目录）
./shareclip-server

# 2) 每台要共享剪贴板的机器启动 client（Windows / Linux 都如此）
./shareclip-client -s 192.168.1.10:9000

# 3) 正常复制/粘贴即可。可选：浏览器打开 http://192.168.1.10:9000/
#    查看历史、点“推送到全部在线客户端”把某条内容重新推到所有机器。
```

启动参数都有单字母简写，日常用短的就够；`-h` 随时看完整说明。

### server 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-a` | `--addr` | `:9000` | 监听地址，HTTP 与 WebSocket 共用同一端口 |
| `-d` | `--db` | `shareclip.db` | SQLite 历史数据库文件路径 |
| `-l` | `--history-limit` | `500` | 历史保留条数，超出自动清理最旧 |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本后退出 |

### client 参数

| 简写 | 长写法 | 默认 | 说明 |
|------|--------|------|------|
| `-s` | `--server` | （必填） | server 地址 `host:port` |
| `-n` | `--name` | 主机名 | 在其它机器/Web 页显示的机器名 |
| `-p` | `--poll` | `1000` | 剪贴板轮询间隔毫秒（Linux 生效） |
| `-m` | `--max-payload` | `33554432`(32MiB) | 单条内容最大字节数 |
| `-q` | `--quiet` | false | 减少日志 |
| `-v` | `--version` | — | 打印版本后退出 |

简写与长写法完全等价（`-s`、`-server`、`--server` 三种都能用，取值可写
`-s 1.2.3.4:9000` 或 `-s=1.2.3.4:9000`），因此老命令、开机自启脚本里的
`-server ...` 依旧照原样工作。

## Web 页面

`http://<server>:9000/`（默认监听全部网卡，内网可访问）：

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
- 界面为扁平简洁风：白底卡片 + 1px 细描边，纯色强调（无渐变、无浮起阴影），
  自适应亮色/暗色主题（跟随系统），并适配手机屏幕。

相关接口：`GET /api/status`（在线节点快照 JSON）、`GET /api/events`（SSE
事件流：`status` / `clip` / `cleared`）。

## 代码结构

```
cmd/shareclip-server/   server 入口
cmd/shareclip-client/   client 入口
internal/agent/         client 主逻辑：监听剪切板、收发、自动重连
internal/clipboard/     跨平台剪贴板抽象 + Watcher（防回环/去重语义）
  clipboard_windows.go  Win32：CF_UNICODETEXT / CF_DIB（stdlib syscall）
  clipboard_linux.go    Linux：xclip / wl-clipboard
internal/dib/           纯 Go DIB↔PNG 编解码（Windows 图片用）
internal/cli/           命令行选项：长写法 + 单字母简写共用一个变量
internal/protocol/      消息格式（JSON 头 + 二进制负载）
internal/server/        WS 集线器、广播、SSE 事件、Web API 与内嵌页面
internal/store/         SQLite 历史存储（清理/分页/内容读取）
```

## 测试

```bash
go test -count=1 ./...   # 协议/DIB/Watcher/存储/e2e 广播与推送
```

e2e 测试用真实的 WebSocket 连接验证：广播给除发送者外所有 client、图片内容、
历史入库、Web 内容读取与 Web 推送、清空历史。

## 已知限制（有意为之，v1 范围）

- 明文、无鉴权：仅适合可信局域网；跨公网请自行加 VPN/TLS。
- Linux 轮询延迟与“相同内容重复复制”无法识别（见上文行为约定）。
- X11 下无法廉价枚举剪贴板格式，偶尔会漏发“同时带文本的图片复制”。
- Web 推送只能推送到全部在线 client，暂不支持指定某台机器。
- 断线期间本机复制的内容不会补发（重连后重新复制即可）。
- 历史仅展示；文本/图片之外的格式（如文件列表、富文本 HTML）不处理。

## Roadmap（可能的方向）

- 文件传输、TLS 与简单密码鉴权
- 系统托盘常驻客户端
- Web 推送支持指定机器、批量选择、历史导出/搜索
- 压缩传输（图片 PNG 无损体积可能偏大）

## 许可

[MIT](LICENSE)。英文说明见 [README.en.md](README.en.md)。
