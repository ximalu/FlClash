package zerotier

import "net/netip"

// ---- IP packet parsing (L3, from the Android TUN) ----

// IPv4Src returns the source address of an IPv4 packet.
func IPv4Src(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return addr4(pkt[12:16]), true
}

// IPv4Dst returns the destination address of an IPv4 packet.
func IPv4Dst(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return addr4(pkt[16:20]), true
}

// IPv6Src returns the source address of an IPv6 packet.
func IPv6Src(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 40 || pkt[0]>>4 != 6 {
		return netip.Addr{}, false
	}
	return addr16(pkt[8:24]), true
}

// IPv6Dst returns the destination address of an IPv6 packet.
func IPv6Dst(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 40 || pkt[0]>>4 != 6 {
		return netip.Addr{}, false
	}
	return addr16(pkt[24:40]), true
}

// PacketDest returns the destination IP of an IPv4/IPv6 packet.
func PacketDest(pkt []byte) (netip.Addr, bool) {
	if len(pkt) == 0 {
		return netip.Addr{}, false
	}
	switch pkt[0] >> 4 {
	case 4:
		return IPv4Dst(pkt)
	case 6:
		return IPv6Dst(pkt)
	}
	return netip.Addr{}, false
}

func addr4(b []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
}

func addr16(b []byte) netip.Addr {
	var a [16]byte
	copy(a[:], b)
	return netip.AddrFrom16(a)
}

// ---- MAC helpers ----

// IPv4MulticastMAC maps an IPv4 multicast address to 01:00:5e:xx:xx:xx.
func IPv4MulticastMAC(ip netip.Addr) uint64 {
	b := ip.As4()
	return 0x01005e000000 | uint64(b[1]&0x7f)<<16 | uint64(b[2])<<8 | uint64(b[3])
}

// IPv6MulticastMAC maps an IPv6 multicast address to 33:33:xx:xx:xx:xx.
func IPv6MulticastMAC(ip netip.Addr) uint64 {
	b := ip.As16()
	return 0x333300000000 | uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
}

// SolicitedNodeMAC maps an IPv6 unicast to its solicited-node multicast MAC
// 33:33:ff:xx:xx:xx (RFC 4291 §2.7.1).
func SolicitedNodeMAC(ip netip.Addr) uint64 {
	b := ip.As16()
	return 0x3333ff000000 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
}

// SameSubnet reports whether a and b share the first bits of their address.
func SameSubnet(a, b netip.Addr, bits int) bool {
	if bits < 0 {
		bits = 0
	}
	if a.Is4() != b.Is4() {
		return false
	}
	if a.Is4() {
		if bits > 32 {
			bits = 32
		}
		ab, bb := a.As4(), b.As4()
		return samePrefix(ab[:], bb[:], bits)
	}
	if bits > 128 {
		bits = 128
	}
	ab, bb := a.As16(), b.As16()
	return samePrefix(ab[:], bb[:], bits)
}

func samePrefix(a, b []byte, bits int) bool {
	full := bits / 8
	for i := 0; i < full; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	rem := bits % 8
	if rem == 0 {
		return true
	}
	mask := byte(0xff << (8 - rem))
	return a[full]&mask == b[full]&mask
}

// MacToBytes writes a 48-bit MAC big-endian into out (len >= 6).
func MacToBytes(mac uint64, out []byte) {
	for i := 0; i < 6 && i < len(out); i++ {
		out[i] = byte(mac >> (8 * (5 - i)))
	}
}

// MacFromBytes reads a 48-bit MAC big-endian from b (len >= 6).
func MacFromBytes(b []byte) uint64 {
	var m uint64
	for i := 0; i < 6 && i < len(b); i++ {
		m = m<<8 | uint64(b[i])
	}
	return m
}

// ---- L3 address rewriting (SNAT/DNAT) ----
//
// The Android TUN carries FlClash's internal subnet (e.g. 172.19.0.1/30),
// which is NOT routable inside the ZeroTier network. Packets entering ZT must
// carry the node's ZT-assigned IP as source; replies come back to that ZT IP
// and must be rewritten back to the TUN address before being written to the
// TUN. This mirrors what a NAT router does at the L3 boundary.

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// ipv4HeaderChecksum computes the 16-bit ones-complement checksum of a
// 20-byte IPv4 header (the checksum field itself must be zeroed first).
func ipv4HeaderChecksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(be16(hdr[i : i+2]))
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return uint16(^sum & 0xffff)
}

// csumUpdate applies the RFC 1624 incremental update for one 16-bit word:
//
//	HC' = ~(~HC + ~m + m')
func csumUpdate(old uint16, oldWord, newWord uint16) uint16 {
	sum := uint32(^old & 0xffff)
	sum += uint32(^oldWord & 0xffff)
	sum += uint32(newWord)
	sum = (sum >> 16) + (sum & 0xffff)
	sum += sum >> 16
	return uint16(^sum & 0xffff)
}

// rewriteIPv4SrcDst returns a copy of an IPv4 packet with the source and/or
// destination address rewritten (pass netip.Addr{} to leave a field
// unchanged). The IPv4 header checksum is recomputed and the TCP/UDP checksum
// is updated incrementally (RFC 1624). ICMP checksums do not cover the
// pseudo-header and are left untouched. Returns nil for non-IPv4 packets.
func rewriteIPv4SrcDst(pkt []byte, newSrc, newDst netip.Addr) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return nil
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return nil
	}
	out := make([]byte, len(pkt))
	copy(out, pkt)

	var oldW, newW [4]uint16
	n := 0
	if newSrc.Is4() {
		sb := newSrc.As4()
		oldW[n], newW[n] = be16(out[12:14]), be16(sb[0:2])
		n++
		oldW[n], newW[n] = be16(out[14:16]), be16(sb[2:4])
		n++
		copy(out[12:16], sb[:])
	}
	if newDst.Is4() {
		db := newDst.As4()
		oldW[n], newW[n] = be16(out[16:18]), be16(db[0:2])
		n++
		oldW[n], newW[n] = be16(out[18:20]), be16(db[2:4])
		n++
		copy(out[16:20], db[:])
	}
	if n == 0 {
		return out
	}

	// Recompute the IPv4 header checksum.
	out[10], out[11] = 0, 0
	cs := ipv4HeaderChecksum(out[:ihl])
	out[10], out[11] = byte(cs>>8), byte(cs&0xff)

	// Incrementally fix the L4 checksum (TCP/UDP only; ICMP has no
	// pseudo-header dependency). NOTE: TCP and UDP place the checksum at
	// DIFFERENT offsets: UDP at offset 6, TCP at offset 16 (TCP header:
	// ports 0-3, seq 4-7, ack 8-11, flags 12-13, window 14-15, checksum 16-17).
	proto := out[9]
	l4off := ihl
	var csOff int
	switch proto {
	case 6: // TCP
		csOff = l4off + 16
	case 17: // UDP
		csOff = l4off + 6
	default:
		return out
	}
	if csOff+2 <= len(out) {
		old := be16(out[csOff : csOff+2])
		cur := old
		for i := 0; i < n; i++ {
			cur = csumUpdate(cur, oldW[i], newW[i])
		}
		out[csOff], out[csOff+1] = byte(cur>>8), byte(cur&0xff)
	}
	return out
}
