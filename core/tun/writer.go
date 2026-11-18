//go:build android && cgo

package tun

// Android TUN 写入器。
//
// 背景（2026-11）：旧实现是在帧消费 goroutine 里直接 Write，失败就
// time.Sleep(5ms) 重试一次，仍失败则丢弃。两个毛病：
//   1. 不区分 errno：EAGAIN / EINTR / ENOBUFS / EIO / EBADF 一视同仁，
//      EINTR 白等 5ms，EAGAIN 本该等可写事件却固定 sleep。
//   2. sleep 睡在唯一的帧消费 goroutine 上：EIO 连发 N 个包就阻塞 5N ms，
//      把整个 ZT 入站方向卡住（症状：入站流量停滞、上层只能等 TCP 重传）。
//
// 现在：写入收敛到一个专用 goroutine + 有界队列，按 errno 分类处理，
// 并分别计数（导出到 datapath 摘要），丢弃时记一次诊断上下文（限流）。

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/log"
	"golang.org/x/sys/unix"
)

func logWarnf(format string, args ...any) { log.Warnln(format, args...) }

// tunGen 在每次新建 flowRouter（即每次 VpnService.establish）时递增，
// 用于把 TUN 写失败关联到具体的 fd 生命周期。
var tunGen atomic.Uint64

func nextTunGen() uint64 { return tunGen.Add(1) }

const (
	// tunWriteQueue 是待写入 TUN 的包队列长度。TUN 写入通常很快，队列只用于
	// 吸收突发；满了说明消费者严重落后，直接丢（TCP 会重传，排队只会加重延迟）。
	tunWriteQueue = 512

	// 单个包的最大写入尝试次数（对可恢复错误计数）。
	tunWriteMaxAttempts = 4
)

// TunWriteStats 是 TUN 写入侧的计数器快照。
type TunWriteStats struct {
	OK          uint64
	RetryOK     uint64
	Failed      uint64
	DroppedFull uint64
	Gen         uint64

	EINTR   uint64
	EAGAIN  uint64
	ENOBUFS uint64
	EIO     uint64
	EBADF   uint64
	Other   uint64
}

// tunWriter 拥有唯一向 TUN 写入的 goroutine。
type tunWriter struct {
	f    *os.File
	fd   int
	gen  uint64 // TUN fd 世代号：每次重新 establish 递增，用于把 EIO 关联到生命周期事件
	ch   chan []byte
	stop chan struct{}
	done chan struct{}

	ok          atomic.Uint64
	retryOK     atomic.Uint64
	failed      atomic.Uint64
	droppedFull atomic.Uint64

	eintr   atomic.Uint64
	eagain  atomic.Uint64
	enobufs atomic.Uint64
	eio     atomic.Uint64
	ebadf   atomic.Uint64
	other   atomic.Uint64

	// 诊断日志限流：每 10 秒最多 5 条，其余只计数。
	lastLogWindow atomic.Int64
	logCount      atomic.Uint64
	suppressed    atomic.Uint64
}

func newTunWriter(f *os.File, gen uint64) *tunWriter {
	return &tunWriter{
		f:    f,
		fd:   int(f.Fd()),
		gen:  gen,
		ch:   make(chan []byte, tunWriteQueue),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// enqueue 把包交给写线程。队列满时丢包并计数，绝不阻塞调用方（帧消费路径）。
func (w *tunWriter) enqueue(pkt []byte) {
	select {
	case w.ch <- pkt:
	default:
		w.droppedFull.Add(1)
	}
}

// run 是写线程主体。收到 stop 后把已排队的包尽力写完再退出。
func (w *tunWriter) run() {
	defer close(w.done)
	for {
		select {
		case pkt := <-w.ch:
			w.writeOne(pkt)
		case <-w.stop:
			for {
				select {
				case pkt := <-w.ch:
					w.writeOne(pkt)
				default:
					return
				}
			}
		}
	}
}

func (w *tunWriter) shutdown(timeout time.Duration) {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	select {
	case <-w.done:
	case <-time.After(timeout):
	}
}

// writeOne 按 errno 分类处理单包写入。
func (w *tunWriter) writeOne(pkt []byte) {
	var lastErr error
	backoff := 2 * time.Millisecond
	for attempt := 0; attempt < tunWriteMaxAttempts; attempt++ {
		_, err := w.f.Write(pkt)
		if err == nil {
			if attempt == 0 {
				w.ok.Add(1)
			} else {
				w.retryOK.Add(1)
			}
			return
		}
		lastErr = err
		switch errnoOf(err) {
		case unix.EINTR:
			// 被信号中断：立即重试，不睡眠。
			w.eintr.Add(1)
		case unix.EAGAIN:
			// 非阻塞 fd 暂时不可写：等可写事件（而不是固定 sleep）。
			w.eagain.Add(1)
			w.waitWritable(20 * time.Millisecond)
		case unix.ENOBUFS:
			// 内核缓冲区压力：有界退避。
			w.enobufs.Add(1)
			time.Sleep(backoff)
			backoff *= 2
		case unix.EIO:
			// 设备/接口层面问题（历史上 Android TUN 会周期性出现）：
			// 不当作可恢复错误反复重试，记录 fd 世代号后丢弃。
			w.eio.Add(1)
			w.diag("EIO", pkt, lastErr)
			w.failed.Add(1)
			return
		case unix.EBADF:
			// fd 已关闭/句柄管理错误：重试无意义。
			w.ebadf.Add(1)
			w.diag("EBADF", pkt, lastErr)
			w.failed.Add(1)
			return
		default:
			w.other.Add(1)
			time.Sleep(time.Millisecond)
		}
	}
	w.failed.Add(1)
	w.diag("exhausted", pkt, lastErr)
}

// waitWritable 用 poll 等待 fd 可写（最多 timeout）。
func (w *tunWriter) waitWritable(timeout time.Duration) {
	pfd := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLOUT}}
	ms := int(timeout / time.Millisecond)
	if ms <= 0 {
		ms = 1
	}
	_, _ = unix.Poll(pfd, ms)
}

// diag 限流输出一次写失败上下文：每 10 秒最多 5 条，其余只累加 suppressed。
func (w *tunWriter) diag(kind string, pkt []byte, err error) {
	now := time.Now().Unix()
	window := now / 10 * 10
	if w.lastLogWindow.Load() != window {
		w.lastLogWindow.Store(window)
		w.logCount.Store(0)
	}
	if w.logCount.Add(1) > 5 {
		w.suppressed.Add(1)
		return
	}
	dst, proto := "?", -1
	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		dst = netipString(pkt[16:20])
		proto = int(pkt[9])
	}
	logWarnf("[TUN] write %s gen=%d len=%d dst=%s proto=%d err=%v suppressed=%d",
		kind, w.gen, len(pkt), dst, proto, err, w.suppressed.Load())
}

func (w *tunWriter) snapshot() TunWriteStats {
	return TunWriteStats{
		OK:          w.ok.Load(),
		RetryOK:     w.retryOK.Load(),
		Failed:      w.failed.Load(),
		DroppedFull: w.droppedFull.Load(),
		Gen:         w.gen,
		EINTR:       w.eintr.Load(),
		EAGAIN:      w.eagain.Load(),
		ENOBUFS:     w.enobufs.Load(),
		EIO:         w.eio.Load(),
		EBADF:       w.ebadf.Load(),
		Other:       w.other.Load(),
	}
}

// errnoOf 从 error 中取出 syscall.Errno。
func errnoOf(err error) unix.Errno {
	for err != nil {
		if e, ok := err.(unix.Errno); ok {
			return e
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return 0
}

func netipString(b []byte) string {
	if len(b) != 4 {
		return "?"
	}
	return itoa(int(b[0])) + "." + itoa(int(b[1])) + "." + itoa(int(b[2])) + "." + itoa(int(b[3]))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
