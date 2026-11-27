//go:build cgo

package zerotier

// ZeroTier 事件日志文件（2026-11-27）。
//
// 为什么要单独落盘：ZT 事件原本寄居在 mihomo 的日志通道里 —— 那条通道同时受
// 「日志捕获记录」开关与 mihomo 日志等级过滤，内存缓冲还被业务连接日志挤占
// （实测 500 条只装得下 20–30 分钟，其余全被 [TCP]/[DNS] 顶掉）。而换网排障
// 最缺的恰恰是「几小时之前发生了什么」。
//
// 这里与 zt-datapath.log / zt-diag.log 同构：固定写入、自动滚动、保留一份历史档。
// 不受任何 UI 开关与日志等级影响 —— 证据不能被开关毁掉（设置项副标题已写明）。
// 量级：ZT 事件约 570 行/小时，4 MiB 约装 2.5 天，加 .1 共约 5 天。
//
// 行格式：2026-11-27 20:15:03 [warn] [ZT] node restarted (...)
// 时间戳是 App 本地时间（CST），与 datapath 段的 UTC 不同，别混用。

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	ztEventFileName = "zt-events.log"
	ztEventMaxBytes = 4 << 20 // 4 MiB 滚动，保留一份 .1
)

var (
	ztEventMu   sync.Mutex
	ztEventFile *os.File
	ztEventSize int64
	ztEventPath string
	ztEventHome string
)

// ztEventSetHome 由 start() 调用：事件文件放在 App 私有目录的 logs/ 下。
func ztEventSetHome(home string) {
	ztEventMu.Lock()
	ztEventHome = home
	ztEventMu.Unlock()
}

// ZtEventLogPath 返回当前事件日志路径（未初始化时为空）。
func ZtEventLogPath() string {
	ztEventMu.Lock()
	defer ztEventMu.Unlock()
	return ztEventPath
}

// ZtEventRecord 追加一行 ZT 事件。失败仅静默降级，绝不影响数据面。
func ZtEventRecord(level, msg string) {
	ztEventMu.Lock()
	defer ztEventMu.Unlock()
	if ztEventHome == "" {
		return
	}
	path := filepath.Join(ztEventHome, "logs", ztEventFileName)
	if ztEventFile == nil || ztEventPath != path {
		if ztEventFile != nil {
			_ = ztEventFile.Close()
			ztEventFile = nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		if st, err := f.Stat(); err == nil {
			ztEventSize = st.Size()
		}
		ztEventFile, ztEventPath = f, path
	}
	line := time.Now().Format("2006-01-02 15:04:05") + " [" + level + "] " + msg + "\n"
	if ztEventSize+int64(len(line)) > ztEventMaxBytes {
		_ = ztEventFile.Close()
		ztEventFile = nil
		_ = os.Rename(path, path+".1")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		ztEventFile, ztEventSize = f, 0
	}
	if n, err := ztEventFile.WriteString(line); err == nil {
		ztEventSize += int64(n)
	}
}
