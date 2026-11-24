//go:build cgo

package zerotier

/*
#include "wrapper.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 诊断快照（2026-11-24 用户要求）。
//
// 为什么需要：App 日志缓冲只有 500 条，导出时往往只剩最后几分钟，而数据面
// 故障常常发生在几十分钟～几小时之前。诊断快照不依赖日志等级、不怕日志被
// 清空、按固定周期落盘，导出时随日志一起带上，事后能完整复原时间线。
//
// 每行一个 JSON：
//
//	{"t":"2026-11-24T21:19:23.123","reason":"engine-start",
//	 "status":{ ... RuntimeStatus ... },
//	 "sendErrno":"last=ENETUNREACH(101) ENETUNREACH(101)=42",
//	 "goroutines":37,"restarting":false,"framesTx":12,"framesRx":0}
//
// 读法：status 里已经带了引擎状态 / nodeGen / socketGen / 对端表 / 健康判据 /
// 自愈计数 / 收发与错误分类，外加 sendErrno 的 errno 直方图——「wire tx err」
// 在 datapath 摘要里只是合计值，看不出是本地 buffer 满（EAGAIN/ENOBUFS）还是
// 网络没了（ENETUNREACH/EHOSTUNREACH），而换网排障恰恰靠这个区分。
const (
	diagFileName = "zt-diag.log"
	diagMaxBytes = 2 << 20          // 2 MiB 滚动
	diagFastGap  = 15 * time.Second // 异常/故障期间
	diagSlowGap  = 60 * time.Second // 平时
)

var (
	diagMu     sync.Mutex
	diagFile   *os.File
	diagSize   int64
	diagPath   string
	diagHome   string
	diagLastAt time.Time
	diagLastHl string
)

// diagSetHome 由 start() 调用：诊断文件放在 App 私有目录的 logs/ 下。
func diagSetHome(home string) {
	diagMu.Lock()
	diagHome = home
	diagMu.Unlock()
}

// DiagLogPath 返回诊断快照文件路径（未初始化时为空）。
func DiagLogPath() string {
	diagMu.Lock()
	defer diagMu.Unlock()
	return diagPath
}

func diagAppend(line []byte) {
	diagMu.Lock()
	defer diagMu.Unlock()
	if diagHome == "" {
		return
	}
	path := filepath.Join(diagHome, "logs", diagFileName)
	if diagFile == nil || diagPath != path {
		if diagFile != nil {
			_ = diagFile.Close()
			diagFile = nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		if st, err := f.Stat(); err == nil {
			diagSize = st.Size()
		}
		diagFile, diagPath = f, path
	}
	if diagSize+int64(len(line)) > diagMaxBytes {
		_ = diagFile.Close()
		diagFile = nil
		_ = os.Rename(path, path+".1")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		diagFile, diagSize = f, 0
	}
	if n, err := diagFile.Write(line); err == nil {
		diagSize += int64(n)
	}
}

// DiagSnapshot 写一行诊断快照。reason 说明为什么写这一次
// （engine-start / socket-bind / socket-rebind / node-restart-* / health:* /
// teardown-* / periodic …），事后可以按原因过滤。
func (e *Engine) DiagSnapshot(reason string) {
	rec := map[string]any{
		"t":          time.Now().Format("2006-01-02T15:04:05.000"),
		"reason":     reason,
		"status":     GetRuntimeStatus(),
		"sendErrno":  wireSendErrnoSummary(),
		"goroutines": runtime.NumGoroutine(),
	}
	if e != nil {
		rec["restarting"] = e.restarting.Load()
		rec["framesTx"] = e.ztFramesTx.Load()
		rec["framesRx"] = e.ztFramesRx.Load()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	diagAppend(append(b, '\n'))
}

// diagPeriodic 由 socketWatchdog 每 5s 调一次：健康状态变化立刻补一行；
// 异常（stalled/rebuilding/unknown 或帧冻结 ≥30s）时 15s 一行，平时 60s 一行。
func (e *Engine) diagPeriodic(now time.Time) {
	link := e.linkStats(now)
	hl := link.Health
	anomaly := hl != healthOK && hl != healthIdle
	if link.FramesFrozenMs != nil && *link.FramesFrozenMs >= 30_000 {
		anomaly = true
	}
	gap := diagSlowGap
	if anomaly {
		gap = diagFastGap
	}

	diagMu.Lock()
	lastAt, lastHl := diagLastAt, diagLastHl
	diagMu.Unlock()

	if hl != lastHl {
		e.DiagSnapshot("health:" + hl)
		diagMu.Lock()
		diagLastHl, diagLastAt = hl, now
		diagMu.Unlock()
		return
	}
	if !lastAt.IsZero() && now.Sub(lastAt) < gap {
		return
	}
	diagMu.Lock()
	diagLastAt = now
	diagMu.Unlock()
	e.DiagSnapshot("periodic")
}

// diagDumpGoroutines 把全部 goroutine 栈写进诊断文件。
// teardown 等 goroutine 超时时用：卡在 C core 里和「wg 内的 goroutine 同步
// 调 RestartNode → teardown 等自己」这两种死法，只有栈分得清。
func diagDumpGoroutines(reason string) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	diagAppend([]byte(fmt.Sprintf("---- GOROUTINE DUMP (%s) ----\n%s---- END DUMP ----\n", reason, buf[:n])))
}

// wireSendErrnoSummary 把 sendto() 的 errno 直方图转成可读文本。
func wireSendErrnoSummary() string {
	var hist [64]C.uint64_t
	C.flclashtier_zt_send_errno_stats(&hist[0], C.int(len(hist)))
	parts := make([]string, 0, 8)
	for i, v := range hist {
		if v == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%d)=%d", errnoName(i), i, uint64(v)))
	}
	last := int(C.flclashtier_zt_last_send_errno())
	if len(parts) == 0 {
		return fmt.Sprintf("last=%s(%d)", errnoName(last), last)
	}
	return fmt.Sprintf("last=%s(%d) %s", errnoName(last), last, strings.Join(parts, " "))
}

func errnoName(e int) string {
	switch e {
	case 0:
		return "OK"
	case 1:
		return "EPERM"
	case 9:
		return "EBADF"
	case 11:
		return "EAGAIN"
	case 12:
		return "ENOMEM"
	case 13:
		return "EACCES"
	case 16:
		return "EBUSY"
	case 22:
		return "EINVAL"
	case 32:
		return "EPIPE"
	case 88:
		return "ENOTSOCK"
	case 89:
		return "EDESTADDRREQ"
	case 90:
		return "EMSGSIZE"
	case 91:
		return "EPROTOTYPE"
	case 92:
		return "ENOPROTOOPT"
	case 95:
		return "EOPNOTSUPP"
	case 97:
		return "EAFNOSUPPORT"
	case 98:
		return "EADDRINUSE"
	case 99:
		return "EADDRNOTAVAIL"
	case 100:
		return "ENETDOWN"
	case 101:
		return "ENETUNREACH"
	case 103:
		return "ECONNABORTED"
	case 104:
		return "ECONNRESET"
	case 105:
		return "ENOBUFS"
	case 107:
		return "ENOTCONN"
	case 110:
		return "ETIMEDOUT"
	case 111:
		return "ECONNREFUSED"
	case 113:
		return "EHOSTUNREACH"
	}
	return fmt.Sprintf("errno%d", e)
}
