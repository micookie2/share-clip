package clientsvc

import (
	"sync"
	"time"

	"github.com/micookie2/share-clip/internal/logx"
)

// subBuffer 是每个界面订阅者的缓冲深度。本地界面消费很快，这个缓冲只是为了
// 在浏览器短暂卡顿时不拖慢日志链路。
const subBuffer = 512

// eventHub 汇总服务对外暴露的两样东西：
//
//   - 最近若干条日志的环形缓冲（界面重新打开时能立刻看到历史，不必等新的日志）；
//   - 实时事件订阅（日志 + 状态变化），供界面用 SSE 推送。
//
// 它同时是 logx 的订阅者：所有日志（包括 agent、剪贴板、网络层的）都从同一条
// 链路进来，界面看到的顺序与文件/控制台完全一致。
type eventHub struct {
	mu        sync.Mutex
	capacity  int
	entries   []LogEntry // 环形缓冲，最新在末尾
	nextSeq   uint64
	curStatus Status
	subs      map[int]chan Event
	nextSub   int
}

func newEventHub(capacity int) *eventHub {
	if capacity <= 0 {
		capacity = 500
	}
	h := &eventHub{
		capacity: capacity,
		subs:     map[int]chan Event{},
	}
	logx.Subscribe(h.onLog)
	return h
}

// onLog 由 logx 同步调用，必须尽快返回：这里只做加锁、入环与向订阅者非阻塞
// 投递，绝不做 IO。
func (h *eventHub) onLog(e logx.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextSeq++
	entry := LogEntry{Seq: h.nextSeq, At: e.At, Text: e.Text}
	h.entries = append(h.entries, entry)
	if len(h.entries) > h.capacity {
		// 丢弃最旧的一段；copy 而不是 append，重叠切片一眼能看出是安全的。
		n := copy(h.entries, h.entries[len(h.entries)-h.capacity:])
		h.entries = h.entries[:n]
	}
	h.publishLocked(Event{Kind: "log", Log: &entry})
}

// setStatus 记录并广播一次状态变化。阶段没变时保留进入该阶段的时刻，避免
// 「已连接」的时间戳被心跳式的刷新不断推后。
func (h *eventHub) setStatus(st Status) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if st.Phase != h.curStatus.Phase || h.curStatus.Since.IsZero() {
		st.Since = time.Now()
	} else {
		st.Since = h.curStatus.Since
	}
	h.curStatus = st
	cp := st
	h.publishLocked(Event{Kind: "status", Status: &cp})
}

func (h *eventHub) status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.curStatus
}

// logsAfter 返回序号大于 after 的日志。after 为 0 表示「给我最近的一批」，
// 这样界面一打开就能看到历史而不是空白。
func (h *eventHub) logsAfter(after uint64, limit int) ([]LogEntry, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if after == 0 {
		start := 0
		if limit > 0 && len(h.entries) > limit {
			start = len(h.entries) - limit
		}
		out := make([]LogEntry, len(h.entries)-start)
		copy(out, h.entries[start:])
		return out, h.nextSeq
	}

	out := make([]LogEntry, 0, 8)
	for _, e := range h.entries {
		if e.Seq <= after {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, h.nextSeq
}

// clearLogs 清空环形缓冲并通知所有界面把日志视图一并清空，免得「清空」之后
// 刷新页面又冒出旧记录。
func (h *eventHub) clearLogs() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = nil
	h.publishLocked(Event{Kind: "cleared"})
}

// subscribe 注册一个实时事件流。返回的注销函数可重复调用。
func (h *eventHub) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)

	h.mu.Lock()
	id := h.nextSub
	h.nextSub++
	h.subs[id] = ch
	// 立刻补一条当前状态，界面不必再单独拉一次就能显示指示灯。
	cur := h.curStatus
	h.mu.Unlock()

	ch <- Event{Kind: "status", Status: &cur}

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// publishLocked 向所有订阅者投递事件（调用方必须持有 h.mu）。
//
// 日志可以丢：环形缓冲还在，界面重连后能补齐。状态不能丢，否则指示灯会一直
// 停在旧状态——所以状态事件遇到满缓冲时先腾一格再投。
func (h *eventHub) publishLocked(ev Event) {
	for _, ch := range h.subs {
		select {
		case ch <- ev:
			continue
		default:
		}
		if ev.Kind != "status" {
			continue
		}
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- ev:
		default:
		}
	}
}
