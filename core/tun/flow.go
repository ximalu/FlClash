//go:build android && cgo

package tun

import (
	"bytes"
	"core/zerotier"
	"fmt"
	"github.com/metacubex/mihomo/log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

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
	writer     *tunWriter // 唯一向 TUN 写入的 goroutine（errno 分类 + 计数）

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
	f.writer = newTunWriter(f.tunFile, nextTunGen())
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.writer.run()
	}()
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
			f.logDataPathStats(now)
		}
	}
}

// logDataPathStats 每 10 秒输出一行数据面计数（未变化则沉默）。
// 每包一条 Info 会把帧消费路径拖慢，这才是应该看的可观测数据：
// 哪个阶段在丢、丢了多少。
func (f *flowRouter) logDataPathStats(now time.Time) {
	if !lastStatsAt.IsZero() && now.Sub(lastStatsAt) < 10*time.Second {
		return
	}
	st := f.adapter.Stats()
	fd, ok, ea, en, ot := f.eng.DataPathCounters()
	var tw TunWriteStats
	if f.writer != nil {
		tw = f.writer.snapshot()
	}
	rxP, rxB, pOK, pErr := f.eng.WireRecvCounters()
	cur := [22]uint64{st.FramesRx, st.Snat, st.Dnat, st.Fragmented4, st.Dropped6, fd,
		f.eng.FramesDropped(), tw.OK, tw.RetryOK, tw.Failed, tw.DroppedFull,
		tw.EIO, tw.EAGAIN, tw.ENOBUFS, tw.EBADF, tw.Other, ok, ea + en + ot,
		rxP, rxB, pOK, pErr}
	if cur == lastStatsVals {
		return
	}
	lastStatsVals = cur
	lastStatsAt = now
	rcv, snd := zerotier.UDPSocketBuffers()
	logDataPathf("[ZT] datapath wire rx=%dp/%dB proc=%dok/%de | frames rx=%d snat=%d dnat=%d frag4=%d drop6=%d mss=%d | qdrop C=%d Go=%d | tun ok=%d retryOk=%d fail=%d full=%d gen=%d | tunerr eio=%d eagain=%d enobufs=%d ebadf=%d other=%d | wire tx ok=%d err=%d | ubuf rcv=%d snd=%d",
		rxP, rxB, pOK, pErr,
		st.FramesRx, st.Snat, st.Dnat, st.Fragmented4, st.Dropped6, st.MssClamped, fd, f.eng.FramesDropped(),
		tw.OK, tw.RetryOK, tw.Failed, tw.DroppedFull, tw.Gen,
		tw.EIO, tw.EAGAIN, tw.ENOBUFS, tw.EBADF, tw.Other, ok, ea+en+ot, rcv, snd)
}

// writeTUN 是 adapter 的 Out 回调（ZT 入站 IP 包写回 TUN）。
//
// Android TUN 写偶发 EIO（input/output error）会让入站包丢失（2026-09-09
// 现场：ZT 入站周期性 EIO，TCP 全靠重传兜底 → 访问慢、ping 丢包）。先
// 重试一次兜住瞬时失败；仍失败则记详细诊断上下文（包摘要/fd 状态/接口
// flags）供诊断版定位根因。
// TUN 写入结果计数：首次成功 / 瞬时失败重试成功 / 最终丢弃。ZT 入站包
// 在最后一步丢失时，对端只能靠 TCP 重传，所以这三个数必须可见。
// TUN 写入计数已迁移到 writer.go 的 tunWriter（含 errno 分类）。

func (f *flowRouter) writeTUN(pkt []byte) {
	// 交给专用写线程：绝不在帧消费 goroutine 上阻塞。旧实现在这里内联
	// time.Sleep(5ms) 重试一次，EIO 连发时会把整个 ZT 入站方向卡死。
	// errno 分类、重试策略与计数器见 writer.go。
	if f.writer != nil {
		f.writer.enqueue(pkt)
	}
}

// tunWriteDiag 收集一次 TUN 写失败的上下文：包目标/协议、fd 打开状态、
// 接口是否仍存活（TUNGETIFF 读接口名 + flags）。诊断版用，定位 EIO 根因。
func tunWriteDiag(tf *os.File, pkt []byte) string {
	var b strings.Builder
	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		fmt.Fprintf(&b, "pktLen=%d dst=%d.%d.%d.%d proto=%d src=%d.%d.%d.%d",
			len(pkt), pkt[16], pkt[17], pkt[18], pkt[19], pkt[9], pkt[12], pkt[13], pkt[14], pkt[15])
	} else {
		fmt.Fprintf(&b, "pktLen=%d head=0x%02x%02x%02x%02x", len(pkt), pkt[0], pkt[1], pkt[2], pkt[3])
	}
	fd := tf.Fd()
	if fl, _, e := unix.Syscall(unix.SYS_FCNTL, fd, unix.F_GETFL, 0); e == 0 {
		fmt.Fprintf(&b, " fdFlags=0x%x", fl)
	} else {
		fmt.Fprintf(&b, " fcntl=ERR(%v)", e)
	}
	if name, flags, ok := tunGetIFF(fd); ok {
		fmt.Fprintf(&b, " iface=%s iffFlags=0x%x", name, flags)
	} else {
		fmt.Fprintf(&b, " tungetiff=ERR")
	}
	return b.String()
}

// tunGetIFF 通过 TUNGETIFF ioctl 读 tun 接口名与 flags（判断 fd 是否仍
// 指向存活接口）。
func tunGetIFF(fd uintptr) (string, uint32, bool) {
	var ifr [40]byte
	copy(ifr[:], "flclash")
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, unix.TUNGETIFF, uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 {
		return "", 0, false
	}
	name := string(ifr[:bytes.IndexByte(ifr[:16], 0)])
	flags := uint32(ifr[16]) | uint32(ifr[17])<<8
	return name, flags, true
}

// shutdown 停止所有 goroutine 并按序拆除 ZT engine。幂等。
func (f *flowRouter) shutdown() {
	f.once.Do(func() {
		close(f.stopCh)
		if f.writer != nil {
			f.writer.shutdown(500 * time.Millisecond)
		}
		_ = f.tunFile.Close()
		_ = f.mihomoSock.Close()
		f.wg.Wait()
		if f.eng != nil {
			f.eng.Stop()
		}
		log.Infoln("[TUN] flow router stopped")
	})
}

var (
	lastStatsAt   time.Time
	lastStatsVals [22]uint64
)
