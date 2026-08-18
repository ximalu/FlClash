package zerotier

import (
	"net/netip"
	"testing"
	"time"
)

func mustPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

func mustAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestRouteTableLongestPrefix(t *testing.T) {
	rt := NewRouteTable()
	rt.Set([]Route{
		{Prefix: mustPrefix("192.168.196.0/24")},
		{Prefix: mustPrefix("192.168.196.128/25")},
	})
	if rt.Count() != 2 {
		t.Fatalf("count=%d want 2", rt.Count())
	}
	// longest prefix wins
	if r := rt.Match(mustAddr("192.168.196.200")); r == nil || r.Prefix.Bits() != 25 {
		t.Fatalf("match .200: got %+v want /25", r)
	}
	// other part of /24 still matches /24
	if r := rt.Match(mustAddr("192.168.196.10")); r == nil || r.Prefix.Bits() != 24 {
		t.Fatalf("match .10: got %+v want /24", r)
	}
	// outside
	if r := rt.Match(mustAddr("10.0.0.1")); r != nil {
		t.Fatalf("match 10.0.0.1: got %+v want nil", r)
	}
}

func TestRouteTableMetricTieBreak(t *testing.T) {
	rt := NewRouteTable()
	rt.Set([]Route{
		{Prefix: mustPrefix("192.168.196.0/24"), Metric: 100},
		{Prefix: mustPrefix("192.168.196.0/24"), Metric: 10},
	})
	r := rt.Match(mustAddr("192.168.196.5"))
	if r == nil || r.Metric != 10 {
		t.Fatalf("expected metric 10, got %+v", r)
	}
}

func TestRouteTableIgnoresDefault(t *testing.T) {
	rt := NewRouteTable()
	rt.Set([]Route{
		{Prefix: mustPrefix("0.0.0.0/0")},
		{Prefix: mustPrefix("::/0")},
		{Prefix: mustPrefix("192.168.196.0/24")},
	})
	if rt.Count() != 1 {
		t.Fatalf("count=%d want 1 (defaults filtered)", rt.Count())
	}
	if r := rt.Match(mustAddr("8.8.8.8")); r != nil {
		t.Fatalf("default route must not capture 8.8.8.8, got %+v", r)
	}
}

func TestRouteTableClear(t *testing.T) {
	rt := NewRouteTable()
	rt.Set([]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	rt.Clear()
	if rt.Count() != 0 {
		t.Fatalf("count=%d want 0", rt.Count())
	}
}

func TestNeighTable(t *testing.T) {
	nt := NewNeighTable(100 * time.Millisecond)
	ip := mustAddr("192.168.196.81")
	nt.Learn(ip, 0x123456789abc)
	if got := nt.Get(ip); got != 0x123456789abc {
		t.Fatalf("get=%x want 123456789abc", got)
	}
	time.Sleep(120 * time.Millisecond)
	if got := nt.Get(ip); got != 0 {
		t.Fatalf("expired entry still returned: %x", got)
	}
	nt.Learn(ip, 0x1)
	if n := nt.Cleanup(time.Now()); n != 0 {
		t.Fatalf("cleanup removed %d fresh entries", n)
	}
	time.Sleep(120 * time.Millisecond)
	if n := nt.Cleanup(time.Now()); n != 1 {
		t.Fatalf("cleanup removed %d want 1", n)
	}
}

func TestPendingQueue(t *testing.T) {
	pq := NewPendingQueue()
	ip := mustAddr("192.168.196.81")
	pkt := []byte{1, 2, 3}
	pq.Add(ip, pkt, time.Now().Add(time.Second))
	pq.Add(ip, []byte{4, 5}, time.Now().Add(time.Second))

	var flushed [][]byte
	pq.Flush(ip, func(p []byte) { flushed = append(flushed, p) })
	if len(flushed) != 2 || flushed[0][0] != 1 || flushed[1][0] != 4 {
		t.Fatalf("flush order wrong: %v", flushed)
	}
	if pq.Len() != 0 {
		t.Fatalf("len=%d want 0 after flush", pq.Len())
	}

	pq.Add(ip, pkt, time.Now().Add(-time.Second)) // already expired
	if n := pq.Cleanup(time.Now()); n != 1 {
		t.Fatalf("cleanup=%d want 1", n)
	}
}

func TestARPRoundTrip(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	localIP := mustAddr("192.168.196.88")
	targetIP := mustAddr("192.168.196.81")

	req := BuildARPRequest(localMAC, localIP, targetIP)
	ap := ParseARP(req)
	if ap == nil || ap.Op != ARPRequest || ap.SenderMAC != localMAC ||
		ap.SenderIP != localIP || ap.TargetIP != targetIP {
		t.Fatalf("bad request parse: %+v", ap)
	}

	reply := BuildARPReply(localMAC, localIP, 0x123456789abc, targetIP)
	ap2 := ParseARP(reply)
	if ap2 == nil || ap2.Op != ARPReply || ap2.TargetMAC != 0x123456789abc ||
		ap2.TargetIP != targetIP || ap2.SenderIP != localIP {
		t.Fatalf("bad reply parse: %+v", ap2)
	}

	if ParseARP([]byte{1, 2, 3}) != nil {
		t.Fatal("short packet must not parse")
	}
	bad := make([]byte, 28)
	bad[2] = 0x08 // wrong ether type
	if ParseARP(bad) != nil {
		t.Fatal("wrong ethertype must not parse")
	}
}

func TestNDPBuildParse(t *testing.T) {
	srcMAC := uint64(0x0a0000000001)
	srcIP := mustAddr("fd00::1")
	target := mustAddr("fd00::81")

	ns := BuildNS(srcMAC, srcIP, target)
	if !IsNeighborSolicitation(ns) {
		t.Fatal("NS not detected")
	}
	if got := NSTarget(ns); got != target {
		t.Fatalf("NS target=%s want %s", got, target)
	}
	if got := LinkLayerAddrOption(ns, 1); got != srcMAC {
		t.Fatalf("NS source LL=%x want %x", got, srcMAC)
	}
	// checksum must be consistent: zero it and recompute
	ns2 := append([]byte(nil), ns...)
	ns2[42], ns2[43] = 0, 0
	if cs := icmp6Checksum(ns2); uint16(cs) != uint16(ns[42])<<8|uint16(ns[43]) {
		t.Fatalf("checksum mismatch: computed=%04x stored=%02x%02x", cs, ns[42], ns[43])
	}

	na := BuildNA(srcMAC, srcIP, target, srcMAC)
	if !IsNeighborAdvertisement(na) {
		t.Fatal("NA not detected")
	}
	if got := NATarget(na); got != target {
		t.Fatalf("NA target=%s want %s", got, target)
	}
	if got := LinkLayerAddrOption(na, 2); got != srcMAC {
		t.Fatalf("NA target LL=%x want %x", got, srcMAC)
	}
	na2 := append([]byte(nil), na...)
	na2[42], na2[43] = 0, 0
	if cs := icmp6Checksum(na2); uint16(cs) != uint16(na[42])<<8|uint16(na[43]) {
		t.Fatalf("NA checksum mismatch: computed=%04x stored=%02x%02x", cs, na[42], na[43])
	}
}

func TestSameSubnet(t *testing.T) {
	if !SameSubnet(mustAddr("192.168.196.88"), mustAddr("192.168.196.1"), 24) {
		t.Fatal("same /24 must be true")
	}
	if SameSubnet(mustAddr("192.168.196.88"), mustAddr("192.168.197.1"), 24) {
		t.Fatal("different /24 must be false")
	}
	if !SameSubnet(mustAddr("fd00::1"), mustAddr("fd00::2"), 64) {
		t.Fatal("same /64 must be true")
	}
}

func TestMulticastMACs(t *testing.T) {
	if got := IPv4MulticastMAC(mustAddr("224.0.0.251")); got != 0x01005e0000fb {
		t.Fatalf("v4 mcast mac=%x want 01005e0000fb", got)
	}
	if got := IPv6MulticastMAC(mustAddr("ff02::1")); got != 0x333300000001 {
		t.Fatalf("v6 mcast mac=%x want 333300000001", got)
	}
	if got := SolicitedNodeMAC(mustAddr("fe80::1234:5678:9abc:def1")); got != 0x3333ffbcdef1 {
		t.Fatalf("solicited-node mac=%x want 3333ffbcdef1", got)
	}
}
