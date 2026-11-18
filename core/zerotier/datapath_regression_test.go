//go:build cgo

package zerotier

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// buildIPv4WithL4 builds a minimal IPv4 packet carrying a TCP or UDP header
// followed by payload. If fragOff > 0 the packet is marked as a non-first
// fragment and the "L4 header" area is really just payload bytes.
func buildIPv4WithL4(proto byte, fragOff uint16, payloadLen int) []byte {
	ihl := 20
	total := ihl + payloadLen
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	binary.BigEndian.PutUint16(pkt[6:8], fragOff&0x1fff)
	pkt[8] = 64
	pkt[9] = proto
	copy(pkt[12:16], []byte{172, 19, 0, 1})
	copy(pkt[16:20], []byte{192, 168, 196, 126})
	if fragOff == 0 {
		// Fill the transport header area with recognisable values.
		for i := ihl; i < total; i++ {
			pkt[i] = byte(i)
		}
	}
	return pkt
}

// A non-first fragment carries no TCP/UDP header: rewriting bytes at the L4
// checksum offset would corrupt user data and break reassembly. Only the IP
// header may change.
func TestRewriteNonFirstFragmentLeavesPayloadIntact(t *testing.T) {
	pkt := buildIPv4WithL4(6 /*TCP*/, 370 /* 2960/8 */, 100)
	// Paint payload with a known pattern so corruption is obvious.
	for i := 20; i < len(pkt); i++ {
		pkt[i] = 0xA5
	}
	orig := append([]byte(nil), pkt...)

	rw := rewriteIPv4SrcDst(pkt, netip.Addr{}, netip.MustParseAddr("172.19.0.1"))
	if rw == nil {
		t.Fatal("rewriteIPv4SrcDst returned nil")
	}
	// Destination must be rewritten.
	for i := 0; i < 4; i++ {
		if rw[16+i] != []byte{172, 19, 0, 1}[i] {
			t.Fatalf("destination not rewritten: %v", rw[16:20])
		}
	}
	// Payload must be byte-for-byte identical.
	for i := 20; i < len(orig); i++ {
		if rw[i] != orig[i] {
			t.Fatalf("non-first fragment payload corrupted at offset %d: %#x != %#x", i, rw[i], orig[i])
		}
	}
	// IPv4 header checksum must be valid: the ones-complement sum over the
	// header (including the checksum field itself) must fold to zero.
	if got := ipv4HeaderChecksum(rw[:20]); got != 0 {
		t.Fatalf("header checksum does not verify: sum folds to %#x, want 0", got)
	}
}

// UDP checksum 0 means "not computed" and must stay 0.
func TestRewritePreservesZeroUDPChecksum(t *testing.T) {
	pkt := buildIPv4WithL4(17 /*UDP*/, 0, 20)
	binary.BigEndian.PutUint16(pkt[20+6:20+8], 0) // UDP checksum = 0
	rw := rewriteIPv4SrcDst(pkt, netip.MustParseAddr("192.168.196.126"), netip.Addr{})
	if rw == nil {
		t.Fatal("nil rewrite")
	}
	if got := binary.BigEndian.Uint16(rw[20+6 : 20+8]); got != 0 {
		t.Fatalf("UDP zero checksum was rewritten to %#x", got)
	}
}

// A non-zero UDP checksum must still be updated when the first fragment's
// addresses are rewritten.
func TestRewriteUpdatesNonZeroUDPChecksum(t *testing.T) {
	pkt := buildIPv4WithL4(17, 0, 20)
	binary.BigEndian.PutUint16(pkt[20+6:20+8], 0x1234)
	rw := rewriteIPv4SrcDst(pkt, netip.MustParseAddr("192.168.196.126"), netip.Addr{})
	if rw == nil {
		t.Fatal("nil rewrite")
	}
	if got := binary.BigEndian.Uint16(rw[20+6 : 20+8]); got == 0x1234 {
		t.Fatalf("UDP checksum was not updated (still %#x)", got)
	}
}

// ipv4HeaderChecksum must cover IP options (ihl > 5).
func TestIPv4HeaderChecksumCoversOptions(t *testing.T) {
	hdr := make([]byte, 24) // 4 bytes of options
	hdr[0] = 0x46           // version 4, ihl 6
	binary.BigEndian.PutUint16(hdr[2:4], 40)
	hdr[8] = 64
	hdr[9] = 6
	copy(hdr[12:16], []byte{10, 0, 0, 1})
	copy(hdr[16:20], []byte{10, 0, 0, 2})
	want := ipv4HeaderChecksum(hdr)

	// Mutate an OPTION byte: a 20-byte-only implementation would not notice.
	hdr[21] ^= 0xFF
	if got := ipv4HeaderChecksum(hdr); got == want {
		t.Fatal("checksum ignored IP option bytes (still summing only 20 bytes)")
	}
}

// MTU handling must honour every legal ZeroTier network MTU (1280..10000) and
// only fall back to the 2800 default for out-of-range values.
func TestSendFrameHonoursLegalMTU(t *testing.T) {
	for _, mtu := range []int{1280, 1400, 1500, 2800, 9000} {
		got := effectiveMTU(mtu)
		if got != mtu {
			t.Fatalf("effectiveMTU(%d) = %d, want %d", mtu, got, mtu)
		}
	}
	for _, mtu := range []int{0, -1, 100, 1279, 10001, 65535} {
		if got := effectiveMTU(mtu); got != 2800 {
			t.Fatalf("effectiveMTU(%d) = %d, want default 2800", mtu, got)
		}
	}
}

// publishDeadline must keep the EARLIEST of all published deadlines, and no
// other call site may bypass it (SendFrame used to drop the value entirely).
func TestPublishDeadlineKeepsEarliest(t *testing.T) {
	e := &Engine{}
	e.publishDeadline(5000)
	e.publishDeadline(3000)
	e.publishDeadline(9000)
	e.bgMu.Lock()
	got := e.bgDeadline
	e.bgMu.Unlock()
	if got != 3000 {
		t.Fatalf("bgDeadline = %d, want 3000 (earliest)", got)
	}
	e.publishDeadline(0)
	e.publishDeadline(-5)
	e.bgMu.Lock()
	got = e.bgDeadline
	e.bgMu.Unlock()
	if got != 3000 {
		t.Fatalf("non-positive deadlines must be ignored, bgDeadline = %d", got)
	}
}

// The C ring queue must be large enough to absorb a throughput burst, and the
// drop counter / wire-send stats must be reachable from Go.
func TestFrameQueueCapacityAndCounters(t *testing.T) {
	if cap := FrameQueueCapacity(); cap < 512 {
		t.Fatalf("C frame queue is %d, want >= 512", cap)
	}
	e := &Engine{}
	fd, ok, ea, en, ot := e.DataPathCounters()
	if fd != 0 || ok != 0 || ea != 0 || en != 0 || ot != 0 {
		t.Fatalf("counters should start at zero, got %d %d %d %d %d", fd, ok, ea, en, ot)
	}
}
