package zerotier

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// Regression: SNAT/DNAT must update the TCP checksum at offset 16 (NOT 6)
// and must NOT touch the sequence number. Earlier code wrote the increment
// into offset 6 (seq low word) leaving the real checksum stale — every TCP
// connection broke while ICMP (no pseudo-header) still worked.
func TestTCPChecksumOffsetIs16(t *testing.T) {
	pkt := fakeTCPPacket("172.19.0.1", "192.168.196.81")
	ihl := int(pkt[0]&0x0f) * 4

	// Give the segment non-zero seq/ack so tampering is visible.
	binary.BigEndian.PutUint32(pkt[ihl+4:ihl+8], 0xdeadbeef)  // seq
	binary.BigEndian.PutUint32(pkt[ihl+8:ihl+12], 0xcafe0001) // ack
	binary.BigEndian.PutUint16(pkt[ihl+16:ihl+18], 0)         // zero checksum before recompute
	cs := tcpChecksum(mustAddr("172.19.0.1").As4(), mustAddr("192.168.196.81").As4(), pkt[ihl:])
	binary.BigEndian.PutUint16(pkt[ihl+16:ihl+18], cs)

	origCS := binary.BigEndian.Uint16(pkt[ihl+16 : ihl+18])
	origSeq := binary.BigEndian.Uint32(pkt[ihl+4 : ihl+8])

	out := rewriteIPv4SrcDst(pkt, mustAddr("192.168.196.120"), netip.Addr{})
	newCS := binary.BigEndian.Uint16(out[ihl+16 : ihl+18])
	newSeq := binary.BigEndian.Uint32(out[ihl+4 : ihl+8])
	seqLow := binary.BigEndian.Uint16(out[ihl+6 : ihl+8])

	if newSeq != origSeq {
		t.Fatalf("seq tampered: orig=0x%08x new=0x%08x (low word 0x%04x)", origSeq, newSeq, seqLow)
	}
	if newCS == origCS {
		t.Fatalf("TCP checksum field not updated: still 0x%04x", origCS)
	}
	if !verifyTCPChecksum(out) {
		t.Fatalf("TCP checksum invalid after SNAT")
	}
}

// Same for DNAT direction.
func TestTCPChecksumOffsetDNAT(t *testing.T) {
	pkt := fakeTCPPacket("192.168.196.81", "192.168.196.120")
	ihl := int(pkt[0]&0x0f) * 4
	binary.BigEndian.PutUint32(pkt[ihl+4:ihl+8], 0x11223344)
	binary.BigEndian.PutUint32(pkt[ihl+8:ihl+12], 0x55667788)
	binary.BigEndian.PutUint16(pkt[ihl+16:ihl+18], 0)
	cs := tcpChecksum(mustAddr("192.168.196.81").As4(), mustAddr("192.168.196.120").As4(), pkt[ihl:])
	binary.BigEndian.PutUint16(pkt[ihl+16:ihl+18], cs)

	origCS := binary.BigEndian.Uint16(pkt[ihl+16 : ihl+18])
	origSeq := binary.BigEndian.Uint32(pkt[ihl+4 : ihl+8])

	out := rewriteIPv4SrcDst(pkt, netip.Addr{}, mustAddr("172.19.0.1"))
	newCS := binary.BigEndian.Uint16(out[ihl+16 : ihl+18])
	newSeq := binary.BigEndian.Uint32(out[ihl+4 : ihl+8])

	if newSeq != origSeq {
		t.Fatalf("seq tampered on DNAT: orig=0x%08x new=0x%08x", origSeq, newSeq)
	}
	if newCS == origCS {
		t.Fatalf("TCP checksum not updated on DNAT")
	}
	if !verifyTCPChecksum(out) {
		t.Fatalf("TCP checksum invalid after DNAT")
	}
}

// UDP checksum lives at offset 6 — make sure it is still updated there.
func TestUDPChecksumOffset6(t *testing.T) {
	s := mustAddr("172.19.0.1").As4()
	d := mustAddr("192.168.196.81").As4()
	udp := make([]byte, 8+4)    // header + 4B payload
	udp[0], udp[1] = 0x04, 0x00 // sport 1024
	udp[2], udp[3] = 0x00, 0x35 // dport 53
	udp[4], udp[5] = byte(len(udp)>>8), byte(len(udp))
	// UDP checksum with pseudo-header:
	var sum uint32
	words := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	words(s[:])
	words(d[:])
	sum += 17
	sum += uint32(len(udp))
	words(udp[:6]) // skip checksum field for computation
	words(udp[8:])
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	udpCS := uint16(^sum & 0xffff)
	udp[6], udp[7] = byte(udpCS>>8), byte(udpCS&0xff)

	pkt := make([]byte, 20+len(udp))
	pkt[0] = 0x45
	pkt[2], pkt[3] = byte(len(pkt)>>8), byte(len(pkt))
	pkt[8] = 64
	pkt[9] = 17
	copy(pkt[12:16], s[:])
	copy(pkt[16:20], d[:])
	copy(pkt[20:], udp)
	ihcs := ipv4HeaderChecksum(pkt[:20])
	pkt[10], pkt[11] = byte(ihcs>>8), byte(ihcs&0xff)

	ihl := int(pkt[0]&0x0f) * 4
	origCS := binary.BigEndian.Uint16(pkt[ihl+6 : ihl+8])
	out := rewriteIPv4SrcDst(pkt, mustAddr("192.168.196.120"), netip.Addr{})
	newCS := binary.BigEndian.Uint16(out[ihl+6 : ihl+8])
	if newCS == origCS {
		t.Fatalf("UDP checksum not updated at offset 6")
	}
	// verify: full sum (pseudo + udp incl checksum) folds to 0xffff
	var v uint32
	for i := 0; i+1 < len(out[12:16]); i += 2 {
		v += uint32(out[12+i])<<8 | uint32(out[12+i+1])
	}
	for i := 0; i+1 < len(out[16:20]); i += 2 {
		v += uint32(out[16+i])<<8 | uint32(out[16+i+1])
	}
	v += 17
	v += uint32(len(udp))
	for i := 0; i+1 < len(out[20:]); i += 2 {
		v += uint32(out[20+i])<<8 | uint32(out[20+i+1])
	}
	for v > 0xffff {
		v = (v >> 16) + (v & 0xffff)
	}
	if v != 0xffff {
		t.Fatalf("UDP checksum invalid: fold=0x%04x", v)
	}
}
