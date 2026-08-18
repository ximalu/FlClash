package zerotier

import (
	"testing"
)

// ---- TCP checksum helpers (IPv4 pseudo-header) ----

func tcpChecksumSum(src, dst []byte, tcp []byte) uint32 {
	var sum uint32
	for i := 0; i+1 < len(src); i += 2 {
		sum += uint32(src[i])<<8 | uint32(src[i+1])
	}
	for i := 0; i+1 < len(dst); i += 2 {
		sum += uint32(dst[i])<<8 | uint32(dst[i+1])
	}
	sum += 6 // protocol TCP
	sum += uint32(len(tcp))
	for i := 0; i+1 < len(tcp); i += 2 {
		sum += uint32(tcp[i])<<8 | uint32(tcp[i+1])
	}
	if len(tcp)%2 == 1 {
		sum += uint32(tcp[len(tcp)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return sum
}

func tcpChecksum(src, dst [4]byte, tcp []byte) uint16 {
	s, d := src[:], dst[:]
	sum := tcpChecksumSum(s, d, tcp)
	return uint16(^sum & 0xffff)
}

// verifyTCPChecksum reports whether the packet's TCP checksum (including the
// IPv4 pseudo-header) is valid: summing everything must yield 0xffff.
func verifyTCPChecksum(pkt []byte) bool {
	if len(pkt) < 40 || pkt[0]>>4 != 4 || pkt[9] != 6 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+20 {
		return false
	}
	return tcpChecksumSum(pkt[12:16], pkt[16:20], pkt[ihl:]) == 0xffff
}

// fakeTCPPacket builds a minimal IPv4/TCP packet with valid IP and TCP
// checksums. The TCP payload is 8 bytes so the segment is 28 bytes long.
func fakeTCPPacket(src, dst string) []byte {
	s := mustAddr(src).As4()
	d := mustAddr(dst).As4()
	tcp := make([]byte, 28)
	tcp[0], tcp[1] = 0x30, 0x39 // src port 12345
	tcp[2], tcp[3] = 0x00, 0x50 // dst port 80
	tcp[12] = 0x50              // data offset 5
	tcp[13] = 0x02              // SYN
	tcp[14], tcp[15] = 0x10, 0x00
	cs := tcpChecksum(s, d, tcp)
	tcp[16], tcp[17] = byte(cs>>8), byte(cs&0xff)

	pkt := make([]byte, 20+len(tcp))
	pkt[0] = 0x45
	pkt[2], pkt[3] = byte(len(pkt)>>8), byte(len(pkt))
	pkt[8] = 64
	pkt[9] = 6
	copy(pkt[12:16], s[:])
	copy(pkt[16:20], d[:])
	copy(pkt[20:], tcp)
	ihcs := ipv4HeaderChecksum(pkt[:20])
	pkt[10], pkt[11] = byte(ihcs>>8), byte(ihcs&0xff)
	return pkt
}

// ---- SNAT: TUN (172.19.0.1) → ZT (192.168.196.120) ----

func TestAdapterSNATOutbound(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.120"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.SetTUNAddress(mustAddr("172.19.0.1"))
	a.arp.Learn(mustAddr("192.168.196.81"), peerMAC)

	pkt := fakeTCPPacket("172.19.0.1", "192.168.196.81")
	if err := a.SendIP(mustAddr("192.168.196.81"), pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1", len(fs.frames))
	}
	fr := fs.frames[0]
	src, ok := IPv4Src(fr.Data)
	if !ok || src != mustAddr("192.168.196.120") {
		t.Fatalf("SNAT source = %v, want 192.168.196.120", src)
	}
	if !verifyTCPChecksum(fr.Data) {
		t.Fatal("TCP checksum invalid after SNAT")
	}
}

func TestAdapterNoSNATWhenAlreadyAssigned(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.120"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.SetTUNAddress(mustAddr("172.19.0.1"))
	a.arp.Learn(mustAddr("192.168.196.81"), peerMAC)

	// Source already the ZT IP: must pass through byte-identical.
	pkt := fakeTCPPacket("192.168.196.120", "192.168.196.81")
	if err := a.SendIP(mustAddr("192.168.196.81"), pkt); err != nil {
		t.Fatal(err)
	}
	fr := fs.frames[0]
	src, _ := IPv4Src(fr.Data)
	if src != mustAddr("192.168.196.120") {
		t.Fatalf("unexpected rewrite: src=%v", src)
	}
	if !verifyTCPChecksum(fr.Data) {
		t.Fatal("TCP checksum invalid")
	}
}

// ---- DNAT: ZT (192.168.196.120) → TUN (172.19.0.1) ----

func TestAdapterDNATInbound(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.120"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.SetTUNAddress(mustAddr("172.19.0.1"))

	var got []byte
	a.Out = func(pkt []byte) { got = pkt }

	in := fakeTCPPacket("192.168.196.81", "192.168.196.120")
	a.HandleFrame(Frame{Nwid: 0xb6079f73c6c0eb31, SrcMAC: peerMAC, DstMAC: localMAC, EtherType: EtherTypeIPv4, Data: in})
	if got == nil {
		t.Fatal("no packet forwarded to TUN")
	}
	dst, ok := IPv4Dst(got)
	if !ok || dst != mustAddr("172.19.0.1") {
		t.Fatalf("DNAT destination = %v, want 172.19.0.1", dst)
	}
	if !verifyTCPChecksum(got) {
		t.Fatal("TCP checksum invalid after DNAT")
	}
}

func TestAdapterNoDNATForOtherDst(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.120"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.SetTUNAddress(mustAddr("172.19.0.1"))

	var got []byte
	a.Out = func(pkt []byte) { got = pkt }

	// Destination is another member, not our ZT IP: no rewrite.
	in := fakeTCPPacket("192.168.196.81", "192.168.196.68")
	a.HandleFrame(Frame{Nwid: 0xb6079f73c6c0eb31, SrcMAC: peerMAC, DstMAC: localMAC, EtherType: EtherTypeIPv4, Data: in})
	if got == nil {
		t.Fatal("no packet forwarded to TUN")
	}
	dst, _ := IPv4Dst(got)
	if dst != mustAddr("192.168.196.68") {
		t.Fatalf("unexpected rewrite: dst=%v", dst)
	}
	if !verifyTCPChecksum(got) {
		t.Fatal("TCP checksum invalid")
	}
}
