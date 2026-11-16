package zerotier

// MSS clamping（只对命中 ZT 路由的 TCP）。
//
// 背景（2026-11 实测）：TUN 的 MTU 是 9000，而 ZT 网络 MTU 是 2800。
// 一个 9000 字节的 TCP 段会被我们拆成 4 个 IP 分片（≈7 个 ZT 线包），
// 任意一片丢失 → 整个段作废 → 上层只能等 TCP 重新发送。
//
// 在同一条真实 ZT 链路 + tc netem 注入丢包下实测送达率：
//
//	       9000B(分片)   1400B(不分片)
//	2%        54.8%          92.2%
//	5%        38.4%          85.1%
//	10%       46.4%          76.4%
//	20%       36.3%          49.7%
//
// 所以在握手时就把对端的 MSS 压到 ZT 链路能装下的大小，让 TCP 一开始就
// 不产生需要分片的包。MSS clamp 不取代分片能力（UDP/ICMP 仍然要走分片），
// 它只是不让 TCP 自己制造大包。

// clampTCPMSS 把 IPv4 TCP SYN / SYN-ACK 中的 MSS 选项下调到 ≤ target。
// 只处理未分片的包；找不到 MSS option 或已经是更小值时不动。
// 返回 true 表示确实改过（并已增量修正 TCP 校验和）。
func clampTCPMSS(pkt []byte, target uint16) bool {
	if target == 0 || len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return false
	}
	if pkt[9] != 6 { // 只处理 TCP（UDP/ICMP 不受 MSS 影响）
		return false
	}
	fo := be16(pkt[6:8])
	if fo&0x1fff != 0 || fo&0x2000 != 0 { // 非首片 / 已分片：不动
		return false
	}
	thl := int(pkt[ihl+12]>>4) * 4
	if thl < 20 || ihl+thl > len(pkt) {
		return false
	}
	if pkt[ihl+13]&0x02 == 0 { // 只有 SYN / SYN-ACK 才含 MSS option
		return false
	}

	end := ihl + thl
	for i := ihl + 20; i < end; {
		kind := pkt[i]
		if kind == 0 { // End of options
			break
		}
		if kind == 1 { // NOP
			i++
			continue
		}
		if i+1 >= end {
			break
		}
		l := int(pkt[i+1])
		if l < 2 || i+l > end {
			break
		}
		if kind == 2 && l == 4 { // MSS option
			cur := be16(pkt[i+2 : i+4])
			if cur == 0 || cur <= target {
				return false
			}
			pkt[i+2], pkt[i+3] = byte(target>>8), byte(target)
			// TCP 校验和是增量式的：替换一个 16-bit 字，按 RFC 1624 修正。
			if csOff := ihl + 16; csOff+2 <= len(pkt) {
				c := csumUpdate(be16(pkt[csOff:csOff+2]), cur, target)
				pkt[csOff], pkt[csOff+1] = byte(c>>8), byte(c)
			}
			return true
		}
		i += l
	}
	return false
}

// mssTargetFor 根据 ZT 网络 MTU 算安全 MSS：MTU - IPv4 头(20) - TCP 头(20)。
// 留 4 字节余量给可能的 IP options。
func mssTargetFor(mtu int) uint16 {
	if mtu <= 0 {
		return 0
	}
	t := mtu - 40 - 4
	if t < 536 { // RFC 791 最小合理 MSS
		return 536
	}
	if t > 65495 {
		return 65495
	}
	return uint16(t)
}
