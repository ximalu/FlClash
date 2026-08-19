package zerotier

import (
	"net/netip"
	"testing"
	"time"
)

// fakeSender is an in-memory FrameSender for adapter tests.
type fakeSender struct {
	snap   Snapshot
	routes *RouteTable
	frames []Frame
	subs   []uint64
}

func newFakeSender(mac uint64, assigned []AssignedAddr, routes []Route) *fakeSender {
	rt := NewRouteTable()
	rt.Set(routes)
	return &fakeSender{
		snap:   Snapshot{Nwid: 0xb6079f73c6c0eb31, Status: 1, Mac: mac, MTU: 2800, Assigned: assigned, Routes: routes},
		routes: rt,
	}
}

func (f *fakeSender) Current() (Snapshot, bool) {
	if f.snap.Nwid == 0 {
		return Snapshot{}, false
	}
	return f.snap, true
}
func (f *fakeSender) MatchRoute(addr netip.Addr) *Route { return f.routes.Match(addr) }
func (f *fakeSender) SendFrame(fr Frame) error {
	f.frames = append(f.frames, fr)
	return nil
}
func (f *fakeSender) SubscribeMulticast(nwid, mac uint64, adi uint32) error {
	f.subs = append(f.subs, mac)
	return nil
}

func fakeIPv4Ping(src, dst string) []byte {
	// minimal valid IPv4 header + 8 bytes payload (like an ICMP echo request)
	s := mustAddr(src).As4()
	d := mustAddr(dst).As4()
	pkt := make([]byte, 28)
	pkt[0] = 0x45
	pkt[2], pkt[3] = 0, 28 // total length
	pkt[8] = 64            // TTL
	pkt[9] = 1             // ICMP
	copy(pkt[12:16], s[:])
	copy(pkt[16:20], d[:])
	copy(pkt[20:28], []byte{8, 0, 0, 0, 0, 1, 0, 1}) // echo request body
	return pkt
}

func TestAdapterKnownMACDirectSend(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.arp.Learn(mustAddr("192.168.196.81"), peerMAC)

	pkt := fakeIPv4Ping("192.168.196.88", "192.168.196.81")
	if err := a.SendIP(mustAddr("192.168.196.81"), pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1", len(fs.frames))
	}
	fr := fs.frames[0]
	if fr.EtherType != EtherTypeIPv4 || fr.DstMAC != peerMAC || fr.SrcMAC != localMAC {
		t.Fatalf("bad frame: ether=%04x dst=%012x src=%012x", fr.EtherType, fr.DstMAC, fr.SrcMAC)
	}
}

func TestAdapterARPResolveFlow(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)

	// 1. unknown MAC -> ARP request broadcast + packet queued
	pkt := fakeIPv4Ping("192.168.196.88", "192.168.196.81")
	if err := a.SendIP(mustAddr("192.168.196.81"), pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1 (ARP request)", len(fs.frames))
	}
	arpReq := fs.frames[0]
	if arpReq.EtherType != EtherTypeARP || arpReq.DstMAC != BroadcastMAC {
		t.Fatalf("expected ARP broadcast, got ether=%04x dst=%012x", arpReq.EtherType, arpReq.DstMAC)
	}
	if a.PendingLen() != 1 {
		t.Fatalf("pending=%d want 1", a.PendingLen())
	}

	// 2. ARP reply arrives -> learn + flush queued data frame
	reply := BuildARPReply(peerMAC, mustAddr("192.168.196.81"), localMAC, mustAddr("192.168.196.88"))
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: peerMAC, DstMAC: localMAC, EtherType: EtherTypeARP, Data: reply})
	if a.PendingLen() != 0 {
		t.Fatalf("pending=%d want 0 after flush", a.PendingLen())
	}
	if len(fs.frames) != 2 {
		t.Fatalf("frames=%d want 2 (ARP + data)", len(fs.frames))
	}
	dataFr := fs.frames[1]
	if dataFr.EtherType != EtherTypeIPv4 || dataFr.DstMAC != peerMAC {
		t.Fatalf("flushed frame wrong: ether=%04x dst=%012x", dataFr.EtherType, dataFr.DstMAC)
	}
}

func TestAdapterARPRequestForUs(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)

	// peer asks who-has 192.168.196.88 (our IP) -> we must reply
	req := BuildARPRequest(peerMAC, mustAddr("192.168.196.81"), mustAddr("192.168.196.88"))
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: peerMAC, DstMAC: BroadcastMAC, EtherType: EtherTypeARP, Data: req})
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1 (ARP reply)", len(fs.frames))
	}
	reply := fs.frames[0]
	if reply.EtherType != EtherTypeARP || reply.DstMAC != peerMAC {
		t.Fatalf("bad reply: ether=%04x dst=%012x", reply.EtherType, reply.DstMAC)
	}
	ap := ParseARP(reply.Data)
	if ap == nil || ap.Op != ARPReply || ap.SenderIP != mustAddr("192.168.196.88") || ap.SenderMAC != localMAC {
		t.Fatalf("bad ARP reply content: %+v", ap)
	}
}

func TestAdapterARPRequestNotForUs(t *testing.T) {
	fs := newFakeSender(0x0a0000000001, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	req := BuildARPRequest(0x123456789abc, mustAddr("192.168.196.81"), mustAddr("192.168.196.99"))
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: 0x123456789abc, DstMAC: BroadcastMAC, EtherType: EtherTypeARP, Data: req})
	if len(fs.frames) != 0 {
		t.Fatalf("frames=%d want 0 (not our IP)", len(fs.frames))
	}
	if a.arp.Get(mustAddr("192.168.196.81")) != 0x123456789abc {
		t.Fatal("ARP sender should be learned")
	}
}

func TestAdapterIPv4InboundToTUN(t *testing.T) {
	fs := newFakeSender(0x0a0000000001, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	var got []byte
	a.Out = func(pkt []byte) { got = append([]byte(nil), pkt...) }

	pkt := fakeIPv4Ping("192.168.196.81", "192.168.196.88")
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: 0x123456789abc, DstMAC: 0x0a0000000001, EtherType: EtherTypeIPv4, Data: pkt})
	if got == nil || len(got) == 0 {
		t.Fatal("Out not called")
	}
	if a.arp.Get(mustAddr("192.168.196.81")) != 0x123456789abc {
		t.Fatal("src should be learned from inbound frame")
	}
}

func TestAdapterGatewayRoute(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	gwMAC := uint64(0xaaaaaaaaaaaa)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("10.1.0.0/16"), Via: mustAddr("192.168.196.1")}})
	a := NewAdapter(fs)
	// gateway MAC known, remote dst unknown
	a.arp.Learn(mustAddr("192.168.196.1"), gwMAC)

	pkt := fakeIPv4Ping("192.168.196.88", "10.1.2.3")
	if err := a.SendIP(mustAddr("10.1.2.3"), pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1", len(fs.frames))
	}
	fr := fs.frames[0]
	// must be sent to the GATEWAY's MAC, not ARP for the remote dst
	if fr.DstMAC != gwMAC {
		t.Fatalf("dst mac=%012x want gateway %012x", fr.DstMAC, gwMAC)
	}
}

func TestAdapterDefaultRouteIgnored(t *testing.T) {
	fs := newFakeSender(0x0a0000000001, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("0.0.0.0/0")}})
	a := NewAdapter(fs)
	pkt := fakeIPv4Ping("192.168.196.88", "8.8.8.8")
	if err := a.SendIP(mustAddr("8.8.8.8"), pkt); err == nil {
		t.Fatal("SendIP should fail: /0 route is filtered out, no match")
	}
	if len(fs.frames) != 0 {
		t.Fatalf("frames=%d want 0 (default route must not capture)", len(fs.frames))
	}
}

func TestAdapterFragmentation(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("192.168.196.88"), Bits: 24}},
		[]Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.arp.Learn(mustAddr("192.168.196.81"), peerMAC)

	// build a 4000-byte UDP-ish IPv4 packet (MTU in fake is 2800)
	pkt := make([]byte, 4000)
	pkt[0] = 0x45
	s := mustAddr("192.168.196.88").As4()
	d := mustAddr("192.168.196.81").As4()
	copy(pkt[12:16], s[:])
	copy(pkt[16:20], d[:])
	pkt[2], pkt[3] = byte(4000>>8), byte(4000&0xff)
	for i := 20; i < len(pkt); i++ {
		pkt[i] = byte(i)
	}
	if err := a.SendIP(mustAddr("192.168.196.81"), pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) < 2 {
		t.Fatalf("frames=%d want >=2 fragments", len(fs.frames))
	}
	total := 0
	for i, fr := range fs.frames {
		if fr.EtherType != EtherTypeIPv4 {
			t.Fatalf("fragment %d wrong ethertype", i)
		}
		if len(fr.Data) > 2800 {
			t.Fatalf("fragment %d too big: %d", i, len(fr.Data))
		}
		// RFC 791: each fragment's IPv4 header checksum must be valid after
		// total length / flags+offset rewrite (regression for the missing
		// checksum recompute that silently corrupted all fragmented flows).
		ihl := int(fr.Data[0]&0x0f) * 4
		if ihl < 20 || ihl > len(fr.Data) {
			t.Fatalf("fragment %d bad ihl %d", i, ihl)
		}
		cs := ipv4HeaderChecksum(fr.Data[:ihl])
		got := uint16(fr.Data[10])<<8 | uint16(fr.Data[11])
		if cs != 0 && got != cs {
			t.Fatalf("fragment %d header checksum = 0x%04x want 0x%04x", i, got, cs)
		}
		total += len(fr.Data)
	}
	// payload bytes must survive (approx: headers duplicated per fragment)
	if total <= 4000 {
		t.Fatalf("total fragment bytes %d <= 4000", total)
	}
}

func TestAdapterAssignedSubscriptions(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	fs := newFakeSender(localMAC, []AssignedAddr{
		{Addr: mustAddr("192.168.196.99"), Bits: 24},
		{Addr: mustAddr("fd00::99"), Bits: 64},
	}, []Route{{Prefix: mustPrefix("192.168.196.0/24")}})
	a := NewAdapter(fs)
	a.Cleanup(time.Now())
	a.Cleanup(time.Now()) // second call must not duplicate

	// IPv4 assigned -> broadcast MAC group with ADI = big-endian IP
	if len(fs.subs) != 2 {
		t.Fatalf("subs=%d want 2", len(fs.subs))
	}
	// subs only records MACs; verify via SubscribedGroups dedup
	if a.SubscribedGroups() != 2 {
		t.Fatalf("subscribed=%d want 2", a.SubscribedGroups())
	}
	// ARP ADI for 192.168.196.99 = 0xc0a8c463
	if got := arpADI(mustAddr("192.168.196.99")); got != 0xc0a8c463 {
		t.Fatalf("arpADI=%08x want c0a8c463", got)
	}
	// IPv6 assigned -> solicited-node MAC
	if got := SolicitedNodeMAC(mustAddr("fd00::99")); got != 0x3333ff000099 {
		t.Fatalf("v6 group mac=%x want 3333ff000099", got)
	}
}

func TestAdapterIPv6NeighborFlow(t *testing.T) {
	localMAC := uint64(0x0a0000000001)
	peerMAC := uint64(0x123456789abc)
	fs := newFakeSender(localMAC, []AssignedAddr{{Addr: mustAddr("fd00::88"), Bits: 64}},
		[]Route{{Prefix: mustPrefix("fd00::/64")}})
	a := NewAdapter(fs)

	dst := mustAddr("fd00::81")
	// 1. unknown -> NS to solicited-node multicast + queued
	// minimal IPv6 packet (ICMPv6 echo-ish; only header needed for routing)
	pkt := make([]byte, 48)
	pkt[0] = 0x60
	s6 := mustAddr("fd00::88").As16()
	d6 := dst.As16()
	copy(pkt[8:24], s6[:])
	copy(pkt[24:40], d6[:])
	pkt[6] = 58
	if err := a.SendIP(dst, pkt); err != nil {
		t.Fatal(err)
	}
	if len(fs.frames) != 1 {
		t.Fatalf("frames=%d want 1 (NS)", len(fs.frames))
	}
	ns := fs.frames[0]
	if ns.EtherType != EtherTypeIPv6 || !IsNeighborSolicitation(ns.Data) {
		t.Fatalf("expected NS frame, ether=%04x", ns.EtherType)
	}
	if a.PendingLen() != 1 {
		t.Fatalf("pending=%d want 1", a.PendingLen())
	}

	// 2. NA arrives -> learn + flush
	na := BuildNA(peerMAC, dst, dst, peerMAC)
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: peerMAC, DstMAC: localMAC, EtherType: EtherTypeIPv6, Data: na})
	if a.PendingLen() != 0 {
		t.Fatalf("pending=%d want 0", a.PendingLen())
	}
	if len(fs.frames) != 2 {
		t.Fatalf("frames=%d want 2 (NS + data)", len(fs.frames))
	}
	if fs.frames[1].EtherType != EtherTypeIPv6 || fs.frames[1].DstMAC != peerMAC {
		t.Fatalf("flushed v6 frame wrong")
	}

	// 3. inbound NS for our IP -> NA reply
	a.ndp.Learn(dst, peerMAC) // not needed; NS handler answers directly
	nsForUs := BuildNS(peerMAC, dst, mustAddr("fd00::88"))
	before := len(fs.frames)
	a.HandleFrame(Frame{Nwid: fs.snap.Nwid, SrcMAC: peerMAC, DstMAC: SolicitedNodeMAC(mustAddr("fd00::88")), EtherType: EtherTypeIPv6, Data: nsForUs})
	if len(fs.frames) != before+1 {
		t.Fatalf("frames=%d want %d (NA reply)", len(fs.frames), before+1)
	}
	naReply := fs.frames[len(fs.frames)-1]
	if naReply.EtherType != EtherTypeIPv6 || !IsNeighborAdvertisement(naReply.Data) {
		t.Fatalf("expected NA reply")
	}
	if got := NATarget(naReply.Data); got != mustAddr("fd00::88") {
		t.Fatalf("NA target=%s", got)
	}
	if a.ndp.Get(dst) != peerMAC {
		t.Fatal("NS source should be learned")
	}
}
