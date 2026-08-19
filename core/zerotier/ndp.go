package zerotier

import "net/netip"

// ICMPv6 Neighbor Discovery (RFC 4861). NS/NA packets are full IPv6 packets
// (etherType 0x86DD) whose payload is ICMPv6.
//
// IPv6 header (40B): ver/tc/flow(4) len(2) next(1) hop(1) src(16) dst(16)
// ICMPv6 (8B): type(1) code(1) checksum(2) reserved/flags(4)
// NS/NA target: 16B at offset 48
// ND options start at offset 64.
const (
	icmp6NeighborSolicitation  = 135
	icmp6NeighborAdvertisement = 136
	ipv6NextHeaderICMPv6       = 58
)

// IsNeighborSolicitation reports whether pkt is an ICMPv6 NS.
func IsNeighborSolicitation(pkt []byte) bool {
	return len(pkt) >= 48 && pkt[0]>>4 == 6 && pkt[6] == ipv6NextHeaderICMPv6 && pkt[40] == icmp6NeighborSolicitation
}

// IsNeighborAdvertisement reports whether pkt is an ICMPv6 NA.
func IsNeighborAdvertisement(pkt []byte) bool {
	return len(pkt) >= 48 && pkt[0]>>4 == 6 && pkt[6] == ipv6NextHeaderICMPv6 && pkt[40] == icmp6NeighborAdvertisement
}

// NSTarget returns the target address of an NS packet.
func NSTarget(pkt []byte) netip.Addr {
	if len(pkt) < 64 {
		return netip.Addr{}
	}
	return addr16(pkt[48:64])
}

// NATarget returns the target address of an NA packet.
func NATarget(pkt []byte) netip.Addr {
	if len(pkt) < 64 {
		return netip.Addr{}
	}
	return addr16(pkt[48:64])
}

// LinkLayerAddrOption extracts the MAC from an ND option (1 = source LL,
// 2 = target LL) if present.
func LinkLayerAddrOption(pkt []byte, optType byte) uint64 {
	off := 64
	for off+8 <= len(pkt) {
		t := pkt[off]
		l := int(pkt[off+1]) * 8 // length in 8-octet units
		if l == 0 {
			return 0
		}
		if t == optType && l >= 2 {
			return MacFromBytes(pkt[off+2 : off+8])
		}
		off += l
	}
	return 0
}

// BuildNS builds an ICMPv6 Neighbor Solicitation (with source link-layer
// address option) asking "who has targetIP?" — sent to the solicited-node
// multicast MAC of targetIP.
func BuildNS(srcMAC uint64, srcIP, targetIP netip.Addr) []byte {
	pkt := make([]byte, 72)
	// IPv6 header
	pkt[0] = 0x60
	pkt[4], pkt[5] = 0, 32 // payload length: ICMPv6(8) + target(16) + option(8)
	pkt[6] = ipv6NextHeaderICMPv6
	pkt[7] = 255 // hop limit
	copy(pkt[8:24], srcIP.AsSlice())
	copy(pkt[24:40], targetIP.AsSlice())
	// ICMPv6 NS
	pkt[40] = icmp6NeighborSolicitation
	copy(pkt[48:64], targetIP.AsSlice())
	// Source link-layer address option
	pkt[64] = 1 // source LL addr
	pkt[65] = 1 // len = 1 (8 octets)
	MacToBytes(srcMAC, pkt[66:72])
	// ICMPv6 checksum (pseudo-header + message; checksum field still zero)
	cs := icmp6Checksum(pkt)
	pkt[42], pkt[43] = byte(cs>>8), byte(cs)
	return pkt
}

// BuildNA builds an ICMPv6 Neighbor Advertisement for targetIP (solicited,
// override) with target link-layer address option.
func BuildNA(srcMAC uint64, srcIP, targetIP netip.Addr, targetMAC uint64) []byte {
	pkt := make([]byte, 72)
	pkt[0] = 0x60
	pkt[4], pkt[5] = 0, 32
	pkt[6] = ipv6NextHeaderICMPv6
	pkt[7] = 255
	copy(pkt[8:24], srcIP.AsSlice())
	copy(pkt[24:40], targetIP.AsSlice())
	// ICMPv6 NA
	pkt[40] = icmp6NeighborAdvertisement
	pkt[44] = 0x60 // flags: S(solicited)=1 O(override)=1
	copy(pkt[48:64], targetIP.AsSlice())
	// Target link-layer address option
	pkt[64] = 2 // target LL addr
	pkt[65] = 1
	MacToBytes(targetMAC, pkt[66:72])
	cs := icmp6Checksum(pkt)
	pkt[42], pkt[43] = byte(cs>>8), byte(cs)
	return pkt
}

// icmp6Checksum computes the ICMPv6 checksum (RFC 4443 §2.3) over the full
// IPv6 packet. The checksum field (pkt[42:44]) must be zero when called.
func icmp6Checksum(pkt []byte) uint16 {
	if len(pkt) < 40 {
		return 0
	}
	var sum uint32
	sum = checksumAdd(sum, pkt[8:24])  // src
	sum = checksumAdd(sum, pkt[24:40]) // dst
	l := uint32(len(pkt) - 40)
	sum = checksumAdd(sum, []byte{byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)})
	sum = checksumAdd(sum, []byte{0, 0, 0, ipv6NextHeaderICMPv6})
	sum = checksumAdd(sum, pkt[40:]) // ICMPv6 message
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func checksumAdd(sum uint32, b []byte) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}
