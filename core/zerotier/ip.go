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
