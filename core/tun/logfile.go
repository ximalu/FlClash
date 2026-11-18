//go:build android && cgo

package tun

// 数据面日志文件：把每 10 秒一行（变化时）的 [ZT] datapath 摘要同时写进
// <app home>/logs/zt-datapath.log，供 App 内一键导出。
//
// 只在「计数有变化」时写，正常使用量极小（每小时最多 360 行），滚动到 2MB
// 自动切一个历史档（.1），保留一份历史，不会无限增长。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

const (
	dpLogMaxBytes = 2 << 20 // 2 MiB
	dpLogKeep     = 1       // 保留 1 个历史档（.1）
)

var (
	dpLogMu   sync.Mutex
	dpLogFile *os.File
	dpLogSize int64
	dpLogPath string
)

// DataPathLogPath 返回当前数据面日志文件路径（未初始化时为空）。
func DataPathLogPath() string {
	dpLogMu.Lock()
	defer dpLogMu.Unlock()
	return dpLogPath
}

func dpLogDir() string {
	dir := filepath.Join(constant.Path.HomeDir(), "logs")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// dpLogWrite 追加一行到日志文件（必要时滚动）。失败仅静默降级，绝不影响数据面。
func dpLogWrite(line string) {
	dpLogMu.Lock()
	defer dpLogMu.Unlock()

	if dpLogFile == nil {
		dpLogPath = filepath.Join(dpLogDir(), "zt-datapath.log")
		f, err := os.OpenFile(dpLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			dpLogPath = ""
			return
		}
		dpLogFile = f
		if st, err := f.Stat(); err == nil {
			dpLogSize = st.Size()
		}
	}

	entry := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	n, err := dpLogFile.WriteString(entry)
	if err != nil {
		return
	}
	dpLogSize += int64(n)

	if dpLogSize > dpLogMaxBytes {
		_ = dpLogFile.Close()
		dpLogFile = nil
		dpLogSize = 0
		// 滚动：删除最旧的，把当前改为 .1
		if dpLogKeep > 0 {
			old := fmt.Sprintf("%s.%d", dpLogPath, dpLogKeep)
			_ = os.Remove(old)
			_ = os.Rename(dpLogPath, old)
		}
	}
}

// logDataPathf 同时写到 App 日志流（日志页可见）和日志文件（可导出）。
func logDataPathf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Infoln("%s", line)
	dpLogWrite(line)
}
