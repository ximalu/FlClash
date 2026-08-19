//go:build android && cgo

package tun

import (
	"errors"
	"io"
	"os"
	"sync"

	"github.com/metacubex/mihomo/log"
	"golang.org/x/sys/unix"
)

const (
	pumpBufSize = 65535   // 与 fdbased.BufConfig 一致，覆盖 MTU 9000
	pumpSockBuf = 1 << 20 // 1MB socketpair 缓冲，降低背压丢包
)

// pump 是 FlClashTier M0 的 TUN 数据泵：
// 在真实 TUN fd 与 sing_tun 之间插入 AF_UNIX SOCK_DGRAM socketpair，
// 两个 goroutine 做纯透传（不分流、不接 ZeroTier、不改 mihomo 核心）。
//
//	真实TUN ──┬── pump.tunToSock ──► socketpair ──► sing_tun/mihomo
//	          └── pump.sockToTun ◄── socketpair ◄── sing_tun/mihomo
//
// M0 里程碑：验证"真实 TUN → pump → socketpair → sing_tun"链路后，
// FlClash 原有网络功能不退化，再进入 M1（ZeroTier 分流）。
type pump struct {
	tunFile *os.File // 真实 TUN fd（来自 VpnService）
	sock    *os.File // socketpair 泵侧端点（另一端交给 sing_tun）
	once    sync.Once
}

// newPump 创建 socketpair 并启动两个透传 goroutine。
// 返回 pump 实例和应交给 sing_tun 的 fd（socketpair 另一端）。
// 注意：返回后 mihomoFd 的所有权转移给 sing_tun（listener.Close 负责关闭），
// pump 不再 close 它；但若调用方在 sing_tun.New 之前失败，必须由调用方关闭。
func newPump(tunFd int) (*pump, int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	// 增大两端缓冲，减少 DGRAM 背压丢包
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pumpSockBuf)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, pumpSockBuf)
	}
	p := &pump{
		tunFile: os.NewFile(uintptr(tunFd), "tun"),
		sock:    os.NewFile(uintptr(fds[0]), "pump-sock"),
	}
	go p.tunToSock()
	go p.sockToTun()
	log.Infoln("[TUN] pump started: real fd=%d socketpair=(%d,%d)", tunFd, fds[0], fds[1])
	return p, fds[1], nil
}

// tunToSock: 真实 TUN → socketpair → sing_tun
func (p *pump) tunToSock() {
	buf := make([]byte, pumpBufSize)
	for {
		n, err := p.tunFile.Read(buf)
		if err != nil {
			if !isPumpFdErr(err) {
				log.Warnln("[TUN] pump tun read: %v", err)
			}
			p.shutdown()
			return
		}
		if _, err := p.sock.Write(buf[:n]); err != nil {
			log.Warnln("[TUN] pump sock write: %v", err)
			p.shutdown()
			return
		}
	}
}

// sockToTun: sing_tun → socketpair → 真实 TUN
func (p *pump) sockToTun() {
	buf := make([]byte, pumpBufSize)
	for {
		n, err := p.sock.Read(buf)
		if err != nil {
			if !isPumpFdErr(err) {
				log.Warnln("[TUN] pump sock read: %v", err)
			}
			p.shutdown()
			return
		}
		if _, err := p.tunFile.Write(buf[:n]); err != nil {
			log.Warnln("[TUN] pump tun write: %v", err)
			p.shutdown()
			return
		}
	}
}

// shutdown 关闭所有 fd；任意一端出错都会触发，另一 goroutine 随即退出。
// 幂等：sync.Once 保证只执行一次。
func (p *pump) shutdown() {
	p.once.Do(func() {
		_ = p.sock.Close()
		_ = p.tunFile.Close()
		log.Infoln("[TUN] pump stopped")
	})
}

// isPumpFdErr 判断是否为"fd 已被关闭"类的正常退出错误（不记 Warn）。
func isPumpFdErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, unix.EBADF)
}
