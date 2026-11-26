//go:build cgo

package zerotier

/*
#include "wrapper.h"
*/
import "C"

import (
	"net/netip"
	"strings"
	"sync"
	"unsafe"
)

// 本地接口地址注册（2026-11-27）
//
// 背景：core 不会自己枚举 Android 的网卡地址，它对「我能从哪些本地地址出去」的认识
// 完全来自对端 SelfAwareness 的反推 —— 而反推需要**已经有包进出**。结果就是：
// 蜂窝网络有 IPv6，但我们从不告诉 core 本机的 v6 地址，它就永远只会往 v4 走，
// 而蜂窝的 v4 是运营商 CGNAT（打洞 3 分钟级）。WiFi 那边虽然只有 v4，但单层 NAT
// 打洞快，所以看起来是「WiFi 快、蜂窝慢」。
//
// 所以 Kotlin 在每次网络变化时把当前优选网络的地址列表送进来，这里逐个交给
// ZT_Node_addLocalInterfaceAddress。core 拿到本地 v6 后就可能直接以 v6 去 HELLO
// 对端（v6 没有 NAT，直连成功率远高于 CGNAT 打洞）。

var (
	pendingLocalMu   sync.Mutex
	pendingLocalAddr []netip.Addr
)

// SetLocalInterfaceAddresses 记录并应用当前物理网络的本地地址（CSV："ip,ip,..."）。
// 引擎未运行时只记录，等 start() 结束再补发 —— Kotlin 往往在 VPN 服务刚起、
// ZT 还没 RUNNING 时就报一次。
func SetLocalInterfaceAddresses(csv string) int {
	addrs := make([]netip.Addr, 0, 4)
	for _, raw := range strings.Split(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if ip, err := netip.ParseAddr(s); err == nil && ip.IsValid() {
			addrs = append(addrs, ip)
		}
	}
	pendingLocalMu.Lock()
	pendingLocalAddr = addrs
	pendingLocalMu.Unlock()
	return applyLocalInterfaceAddresses(addrs)
}

// applyPendingLocalAddrs 在引擎 start() 末尾把之前缓存的地址补发一次。
func applyPendingLocalAddrs() int {
	pendingLocalMu.Lock()
	addrs := pendingLocalAddr
	pendingLocalMu.Unlock()
	return applyLocalInterfaceAddresses(addrs)
}

func applyLocalInterfaceAddresses(addrs []netip.Addr) int {
	e := currentEngine()
	if e == nil || (e.getState() != StateRunning && e.getState() != StateStarting) {
		return 0
	}
	v4, v6 := 0, 0
	for _, ip := range addrs {
		switch {
		case ip.Is4():
			b := ip.As4()
			if rc := int(C.flclashtier_zt_add_local_interface_address(4, (*C.uint8_t)(unsafe.Pointer(&b[0])))); rc == 0 {
				v4++
			}
		case ip.Is6():
			b := ip.As16()
			if rc := int(C.flclashtier_zt_add_local_interface_address(6, (*C.uint8_t)(unsafe.Pointer(&b[0])))); rc == 0 {
				v6++
			}
		}
	}
	if v4+v6 > 0 {
		// hasIpv6 一看就知道：>0 = 当前网络可用 v6，这正是“蜂窝优先 v6”要的证据。
		Infof("[ZT-NET] local addresses registered: v4=%d v6=%d (hasIpv6=%t) list=%q",
			v4, v6, v6 > 0, joinAddrs(addrs))
	}
	return v4 + v6
}

func joinAddrs(addrs []netip.Addr) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ",")
}
