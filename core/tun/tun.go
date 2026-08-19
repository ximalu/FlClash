//go:build android && cgo

package tun

import (
	"core/zerotier"
	"github.com/metacubex/mihomo/constant"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/listener/sing_tun"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// stopper 抽象 pump 与 flowRouter 的关闭接口（tun.go 统一收尾）。
type stopper interface{ shutdown() }

func Start(fd int, stack string, address, dns string, protect func(int)) *sing_tun.Listener {
	var prefix4 []netip.Prefix
	var prefix6 []netip.Prefix
	tunStack, ok := constant.StackTypeMapping[strings.ToLower(stack)]
	if !ok {
		tunStack = constant.TunSystem
	}
	for _, a := range strings.Split(address, ",") {
		a = strings.TrimSpace(a)
		if len(a) == 0 {
			continue
		}
		prefix, err := netip.ParsePrefix(a)
		if err != nil {
			log.Errorln("TUN:", err)
			return nil
		}
		if prefix.Addr().Is4() {
			prefix4 = append(prefix4, prefix)
		} else {
			prefix6 = append(prefix6, prefix)
		}
	}

	var dnsHijack []string
	for _, d := range strings.Split(dns, ",") {
		d = strings.TrimSpace(d)
		if len(d) == 0 {
			continue
		}
		dnsHijack = append(dnsHijack, net.JoinHostPort(d, "53"))
	}

	// FlClashTier M1 数据面：
	//   - zerotier.json 配置了 network-id → flowRouter（ZT 分支 + mihomo 分支）
	//   - 否则 → M0 pump（纯透传，零改动，mihomo-only 场景与上游一致）
	// mihomo 分支在两种模式下完全相同：socketpair 一端交给 sing_tun。
	var dp stopper
	mihomoFd := 0

	// TUN 内部 IPv4 地址（如 172.19.0.1）——adapter 用它做 ZT 出口的
	// SNAT/DNAT，因为 TUN 网段在 ZT 网络里不可路由。
	var tunIPv4 netip.Addr
	if len(prefix4) > 0 {
		tunIPv4 = prefix4[0].Addr()
	}

	home := constant.Path.HomeDir()
	if cfg, err := zerotier.LoadConfig(home); err != nil {
		log.Warnln("TUN zerotier config:", err)
	} else if cfg.Enabled() {
		// P0-3: FlClash 的 TUN 拆除是异步的——handleStopTun 关闭 sing_tun
		// listener 后，flowRouter 的 mihomoLoop 才从 goroutine 里触发
		// eng.Stop()。若新 StartEngine 抢先执行，会幂等拿到 RUNNING 的旧
		// engine，随后旧 flowRouter 的 shutdown 把它停掉，新 flowRouter
		// 就挂在一个已停止的 engine 上。先同步等旧 engine 彻底释放。
		// 超时（3s）后若 engine 仍 RUNNING，StartEngine 按幂等语义返回它。
		zerotier.WaitEngineStopped(3 * time.Second)
		if eng, err := zerotier.StartEngine(*cfg, protect, home); err != nil {
			log.Warnln("TUN zerotier engine:", err)
		} else if fr, err := newFlowRouter(fd, eng, tunIPv4); err != nil {
			log.Warnln("TUN flow router:", err)
			eng.Stop()
		} else {
			dp = fr
			mihomoFd = fr.mihomoFd
			log.Infoln("TUN: ZeroTier flow router active (network %s)", cfg.NetworkID)
		}
	}
	if mihomoFd == 0 {
		p, mfd, err := newPump(fd)
		if err != nil {
			log.Errorln("TUN pump create:", err)
			return nil
		}
		dp = p
		mihomoFd = mfd
	}

	options := LC.Tun{
		Enable:              true,
		Device:              "FlClash",
		Stack:               tunStack,
		DNSHijack:           dnsHijack,
		AutoRoute:           false,
		AutoDetectInterface: false,
		Inet4Address:        prefix4,
		Inet6Address:        prefix6,
		MTU:                 9000,
		FileDescriptor:      mihomoFd,
	}

	listener, err := sing_tun.New(options, tunnel.Tunnel)

	if err != nil {
		log.Errorln("TUN:", err)
		// sing_tun 尚未接管 mihomoFd（New 失败），必须显式关闭，否则泄漏。
		_ = unix.Close(mihomoFd)
		dp.shutdown()
		return nil
	}

	// sing_tun 启动成功后，真实 TUN fd 归数据面所有；
	// listener.Close() 会关闭 socketpair 的 mihomo 端，数据面随即自清理。

	return listener
}
