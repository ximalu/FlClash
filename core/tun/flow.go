//go:build android && cgo

package tun

import (
	"core/zerotier"
	"github.com/metacubex/mihomo/log"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	flowBufSize = 65535 // 与 fdbased.BufConfig 一致，覆盖 MTU 9000
)

// flowRouter 是 FlClashTier M1 的数据面：
//
//	Android TUN ──► tunLoop ──► dst ∈ ZT managed routes? ──► adapter.SendIP ──► ZT core
//	                          └─ else ──► socketpair ──► sing_tun/mihomo
//	socketpair ◄────────────── mihomoLoop (mihomo 回程 → TUN)
//	ZT core frame ──► frameLoop ──► adapter.HandleFrame (ARP/NDP/学习) ──► TUN
//
// 与 M0 pump 的差异：tunLoop 不再纯透传，而是按 ZT managed routes（唯一权威
// 来源，来自 ZT config callback，绝无硬编码网段）分流。mihomo 分支与 M0 完全
// 相同（socketpair 透传），未重新设计。
type flowRouter struct {
	tunFile    *os.File // 真实 TUN fd（来自 VpnService）
	mihomoSock *os.File // socketpair 泵侧端点（另一端交给 sing_tun）
	mihomoFd   int      // 应交给 sing_tun 的 fd
	adapter    *zerotier.Adapter
	eng        *zerotier.Engine

	stopCh chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// newFlowRouter 创建 socketpair 并启动 4 个 goroutine（TUN 读 / mihomo 读 /
// ZT frame / 周期清理）。返回 flowRouter 和应交给 sing_tun 的 fd。
// tunIPv4 是 TUN 的内部 IPv4 地址（如 172.19.0.1），adapter 用它做
// SNAT/DNAT：出站包源地址改写为 ZT assigned IP，入站包目标地址还原。
// 注意：返回后 mihomoFd 的所有权转移给 sing_tun（listener.Close 负责关闭），
// flowRouter 不再 close 它；但若调用方在 sing_tun.New 之前失败，必须由
// 调用方关闭 mihomoFd（见 tun.go 失败路径）。
func newFlowRouter(tunFd int, eng *zerotier.Engine, tunIPv4 netip.Addr) (*flowRouter, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, pumpSockBuf)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, pumpSockBuf)
	}
	f := &flowRouter{
		tunFile:    os.NewFile(uintptr(tunFd), "tun"),
		mihomoSock: os.NewFile(uintptr(fds[0]), "flow-sock"),
		mihomoFd:   fds[1],
		eng:        eng,
		adapter:    zerotier.NewAdapter(eng),
		stopCh:     make(chan struct{}),
	}
	f.adapter.Out = f.writeTUN
	f.adapter.SetTUNAddress(tunIPv4)
	f.wg.Add(4)
	go f.tunLoop()
	go f.mihomoLoop()
	go f.frameLoop()
	go f.tickLoop()
	log.Infoln("[TUN] flow router started (ZeroTier enabled): real fd=%d socketpair=(%d,%d)", tunFd, fds[0], fds[1])
	return f, nil
}

// tunLoop: 真实 TUN → 按 managed routes 分流 → ZT / mihomo
func (f *flowRouter) tunLoop() {
	defer f.wg.Done()
	buf := make([]byte, flowBufSize)
	for {
		n, err := f.tunFile.Read(buf)
		if err != nil {
			if !isPumpFdErr(err) {
				log.Warnln("[TUN] flow tun read: %v", err)
			}
			f.shutdown()
			return
		}
		f.routePacket(buf[:n])
	}
}

// routePacket 对单个 IP 包做分流决策。
func (f *flowRouter) routePacket(pkt []byte) {
	dst, ok := zerotier.PacketDest(pkt)
	if ok && f.eng.Ready() && f.eng.MatchRoute(dst) != nil {
		if err := f.adapter.SendIP(dst, pkt); err != nil {
			// 已命中 ZT 路由但发送失败：丢弃（OS 上层会重传），绝不转给 mihomo
			log.Warnln("[TUN] flow ZT send to %s: %v", dst, err)
		}
		return
	}
	if _, err := f.mihomoSock.Write(pkt); err != nil {
		log.Warnln("[TUN] flow mihomo write: %v", err)
		f.shutdown()
		return
	}
}

// mihomoLoop: sing_tun → socketpair → 真实 TUN（与 M0 pump 相同）
func (f *flowRouter) mihomoLoop() {
	defer f.wg.Done()
	buf := make([]byte, flowBufSize)
	for {
		n, err := f.mihomoSock.Read(buf)
		if err != nil {
			if !isPumpFdErr(err) {
				log.Warnln("[TUN] flow sock read: %v", err)
			}
			f.shutdown()
			return
		}
		if _, err := f.tunFile.Write(buf[:n]); err != nil {
			log.Warnln("[TUN] flow tun write: %v", err)
			f.shutdown()
			return
		}
	}
}

// frameLoop: ZT 网络帧 → adapter（ARP/NDP 应答/学习 + IP 载荷 → TUN）
func (f *flowRouter) frameLoop() {
	defer f.wg.Done()
	for {
		select {
		case fr := <-f.eng.Frames():
			f.adapter.HandleFrame(fr)
		case <-f.stopCh:
			return
		}
	}
}

// tickLoop: 周期清理 ARP/NDP 表与 pending 队列
func (f *flowRouter) tickLoop() {
	defer f.wg.Done()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case now := <-t.C:
			f.adapter.Cleanup(now)
		}
	}
}

// writeTUN 是 adapter 的 Out 回调（ZT 入站 IP 包写回 TUN）。
func (f *flowRouter) writeTUN(pkt []byte) {
	if _, err := f.tunFile.Write(pkt); err != nil {
		log.Warnln("[TUN] flow tun write: %v", err)
	}
}

// shutdown 停止所有 goroutine 并按序拆除 ZT engine。幂等。
func (f *flowRouter) shutdown() {
	f.once.Do(func() {
		close(f.stopCh)
		_ = f.tunFile.Close()
		_ = f.mihomoSock.Close()
		f.wg.Wait()
		if f.eng != nil {
			f.eng.Stop()
		}
		log.Infoln("[TUN] flow router stopped")
	})
}
