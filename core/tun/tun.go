//go:build android && cgo

package tun

import "C"
import (
	"github.com/metacubex/mihomo/constant"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/listener/sing_tun"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
	"net"
	"net/netip"
	"strings"
)

func Start(fd int, stack string, address, dns string) *sing_tun.Listener {
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

	// FlClashTier M0: 在真实 TUN 与 sing_tun 之间插入 socketpair 泵（纯透传）。
	// 给 sing_tun 的是 socketpair 一端；泵 goroutine 持真实 TUN fd + 另一端做透传。
	p, mihomoFd, err := newPump(fd)
	if err != nil {
		log.Errorln("TUN pump create:", err)
		return nil
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
		p.shutdown()
		return nil
	}

	// sing_tun 启动成功后，真实 TUN fd 归 pump 所有；
	// listener.Close() 会关闭 socketpair 的 mihomo 端，泵随即自清理。

	return listener
}
