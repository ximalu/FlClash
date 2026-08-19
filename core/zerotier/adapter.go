package zerotier

import (
	"net/netip"
	"time"
)

// Ethernet frame types.
const (
	EtherTypeIPv4 = 0x0800
	EtherTypeARP  = 0x0806
	EtherTypeIPv6 = 0x86DD
)

const (
	// BroadcastMAC is the Ethernet broadcast address.
	BroadcastMAC = 0xffffffffffff
	// neighTTL matches the official ZeroTier Android adapter (120s).
	neighTTL = 120 * time.Second
	// pendingTTL is how long a packet waits for ARP/NDP resolution before
	// being dropped (the OS stack retransmits).
	pendingTTL = 3 * time.Second
)

// Adapter is the L2/L3 conversion layer between the Android TUN (L3) and the
// ZeroTier virtual network (L2). It is the "Flow Router" ZeroTier branch:
//
//	TUN IP packet ──► Adapter.SendIP ──► (route match) ──► ARP/NDP resolve ──► ZT frame
//	ZT frame ────────► Adapter.HandleFrame ──► ARP/NDP answer/learn + IP payload ──► Out (TUN)
//
// Managed routes are consumed ONLY from the ZT config callback (engine
// snapshot) — never hardcoded. Central route changes take effect on the next
// snapshot refresh (engine polls at ~100ms).
//
// Because the Android TUN uses FlClash's internal subnet (172.19.0.1/30) which
// is not routable inside ZT, the adapter source-NATs outgoing IPv4 packets to
// the ZT-assigned IP and destination-NATs incoming packets back to the TUN
// address (SetTUNAddress). See rewriteIPv4SrcDst.
//
// Adapter is pure Go (no cgo) so it is unit-testable with a fake sender.
type Adapter struct {
	eng     FrameSender
	arp     *NeighTable
	ndp     *NeighTable
	pending *PendingQueue

	// Out receives IP packets from the ZT network that must be written to
	// the TUN. Set by the owner before Start.
	Out func(pkt []byte)

	// tunIPv4 is the Android TUN internal IPv4 address (e.g. 172.19.0.1).
	// Set via SetTUNAddress; used for SNAT/DNAT at the L3 boundary.
	tunIPv4 netip.Addr

	// subscribed tracks multicast groups already subscribed (mac<<32|adi),
	// so per-tick maintenance does not spam the core.
	subscribed map[uint64]bool

	// MaxFrame is the ZT network MTU (from config). Larger IPv4 packets are
	// fragmented before entering ZT; larger IPv6 packets are dropped with a
	// warning (no router fragmentation in IPv6).
	fragmented4 int
	dropped6    int
}

// NewAdapter creates the adapter for an engine-backed FrameSender.
func NewAdapter(eng FrameSender) *Adapter {
	return &Adapter{
		eng:        eng,
		arp:        NewNeighTable(neighTTL),
		ndp:        NewNeighTable(neighTTL),
		pending:    NewPendingQueue(),
		subscribed: make(map[uint64]bool),
	}
}

// SetTUNAddress records the Android TUN internal IPv4 address so the adapter
// can SNAT/DNAT between the TUN subnet and the ZT-assigned IP.
func (a *Adapter) SetTUNAddress(ip netip.Addr) { a.tunIPv4 = ip }

// Cleanup drops expired neighbor entries and pending packets, and keeps the
// multicast subscriptions for our assigned addresses current. Call from a
// periodic ticker (1s).
func (a *Adapter) Cleanup(now time.Time) {
	a.arp.Cleanup(now)
	a.ndp.Cleanup(now)
	a.pending.Cleanup(now)
	a.ensureSubscriptions()
}

// ensureSubscriptions subscribes this node to the ARP (IPv4) / NDP (IPv6)
// resolution multicast groups of its assigned addresses, exactly like the
// official ZeroTier clients do. Without this, other members' "who-has
// <our-ip>" queries are never delivered to us (ZeroTier turns plain ARP
// broadcasts into targeted multicasts).
func (a *Adapter) ensureSubscriptions() {
	cfg, ok := a.eng.Current()
	if !ok || cfg.Nwid == 0 {
		return
	}
	for _, aa := range cfg.Assigned {
		var mac uint64
		var adi uint32
		switch {
		case aa.Addr.Is4():
			mac = BroadcastMAC
			b := aa.Addr.As4()
			adi = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		case aa.Addr.Is6():
			mac = SolicitedNodeMAC(aa.Addr)
			adi = 0
		default:
			continue
		}
		key := mac<<32 | uint64(adi)
		if a.subscribed[key] {
			continue
		}
		if err := a.eng.SubscribeMulticast(cfg.Nwid, mac, adi); err != nil {
			Warnf("[ZT] multicastSubscribe mac=%012x adi=%08x: %v", mac, adi, err)
			continue
		}
		a.subscribed[key] = true
		Infof("[ZT] subscribed %s resolution group (mac=%012x adi=%08x)", aa.Addr, mac, adi)
	}
}

// SubscribedGroups returns the number of multicast groups subscribed
// (diagnostics).
func (a *Adapter) SubscribedGroups() int { return len(a.subscribed) }

// arpADI converts an IPv4 address to the ZeroTier ARP multicast ADI
// (the address as a big-endian uint32, per MulticastGroup.hpp
// deriveMulticastGroupForAddressResolution).
func arpADI(ip netip.Addr) uint32 {
	b := ip.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// PendingLen reports queued packets awaiting resolution (diagnostics).
func (a *Adapter) PendingLen() int { return a.pending.Len() }

// ArpLen / NdpLen report neighbor table sizes (diagnostics).
func (a *Adapter) ArpLen() int { return a.arp.Len() }
func (a *Adapter) NdpLen() int { return a.ndp.Len() }

// SendIP routes one TUN IP packet into the ZT network. The packet MUST be
// inside a ZT managed route — otherwise ErrNoRoute is returned and the
// caller (flow router) sends it to mihomo instead. Returns nil if the packet
// was sent or queued for MAC resolution.
func (a *Adapter) SendIP(dst netip.Addr, pkt []byte) error {
	if a.eng.MatchRoute(dst) == nil {
		return ErrNoRoute
	}
	cfg, ok := a.eng.Current()
	if !ok || cfg.Mac == 0 || cfg.Nwid == 0 {
		return ErrNoConfig
	}
	if dst.Is4() {
		return a.sendIPv4(cfg, dst, pkt)
	}
	if dst.Is6() {
		return a.sendIPv6(cfg, dst, pkt)
	}
	return ErrNoConfig
}

func (a *Adapter) sendIPv4(cfg Snapshot, dst netip.Addr, pkt []byte) error {
	pkt = a.snatOut(cfg, pkt)
	if dst.IsMulticast() {
		mac := IPv4MulticastMAC(dst)
		_ = a.eng.SubscribeMulticast(cfg.Nwid, mac, 0)
		return a.sendFrame(cfg, mac, EtherTypeIPv4, pkt)
	}
	// Gateway handling: for a routed subnet (via != direct), packets leaving
	// our local subnet are sent to the gateway's MAC (L3 dst unchanged).
	target := dst
	if r := a.eng.MatchRoute(dst); r != nil && r.HasGateway() {
		if local := firstAssigned4(cfg.Assigned); local.IsValid() && !SameSubnet(dst, local, assignedBits(cfg, local)) {
			target = r.Via
		}
	}
	if mac := a.arp.Get(target); mac != 0 {
		return a.sendFrame(cfg, mac, EtherTypeIPv4, pkt)
	}
	// ARP resolution required.
	local := firstAssigned4(cfg.Assigned)
	if !local.IsValid() {
		return ErrNoLocal
	}
	req := BuildARPRequest(cfg.Mac, local, target)
	Infof("[ZT] ARP request who-has %s from %s (mac=%012x)", target, local, cfg.Mac)
	if err := a.sendFrame(cfg, BroadcastMAC, EtherTypeARP, req); err != nil {
		return err
	}
	a.pending.Add(target, pkt, time.Now().Add(pendingTTL))
	return nil
}

func (a *Adapter) sendIPv6(cfg Snapshot, dst netip.Addr, pkt []byte) error {
	if dst.IsMulticast() {
		mac := IPv6MulticastMAC(dst)
		_ = a.eng.SubscribeMulticast(cfg.Nwid, mac, 0)
		return a.sendFrame(cfg, mac, EtherTypeIPv6, pkt)
	}
	if mac := a.ndp.Get(dst); mac != 0 {
		return a.sendFrame(cfg, mac, EtherTypeIPv6, pkt)
	}
	// Neighbor Solicitation to the solicited-node multicast group.
	local := firstAssigned6(cfg.Assigned)
	if !local.IsValid() {
		return ErrNoLocal
	}
	snMac := SolicitedNodeMAC(dst)
	_ = a.eng.SubscribeMulticast(cfg.Nwid, snMac, 0)
	ns := BuildNS(cfg.Mac, local, dst)
	if err := a.sendFrame(cfg, snMac, EtherTypeIPv6, ns); err != nil {
		return err
	}
	a.pending.Add(dst, pkt, time.Now().Add(pendingTTL))
	return nil
}

// sendFrame handles MTU: IPv4 packets larger than the ZT MTU are fragmented;
// oversized IPv6 packets are dropped (rare; no router fragmentation).
func (a *Adapter) sendFrame(cfg Snapshot, dstMAC uint64, etherType uint16, pkt []byte) error {
	mtu := cfg.MTU
	if mtu <= 0 || mtu > 1500+40 { // sanity: ZT MTU is 2800-ish; never below 1540
		mtu = 2800
	}
	if len(pkt) > mtu {
		if etherType == EtherTypeIPv4 {
			for _, frag := range fragmentIPv4(pkt, mtu) {
				if err := a.eng.SendFrame(Frame{Nwid: cfg.Nwid, SrcMAC: cfg.Mac, DstMAC: dstMAC, EtherType: etherType, Data: frag}); err != nil {
					return err
				}
			}
			a.fragmented4++
			return nil
		}
		a.dropped6++
		Warnf("[ZT] dropping oversized IPv6 packet (%d > MTU %d)", len(pkt), mtu)
		return nil
	}
	return a.eng.SendFrame(Frame{Nwid: cfg.Nwid, SrcMAC: cfg.Mac, DstMAC: dstMAC, EtherType: etherType, Data: pkt})
}

// HandleFrame processes one Ethernet frame received from the ZT network.
func (a *Adapter) HandleFrame(fr Frame) {
	Infof("[ZT] frame rx ether=0x%04x src=%012x dst=%012x len=%d", fr.EtherType, fr.SrcMAC, fr.DstMAC, len(fr.Data))
	switch fr.EtherType {
	case EtherTypeARP:
		a.handleARP(fr)
	case EtherTypeIPv4:
		if src, ok := IPv4Src(fr.Data); ok {
			a.arp.Learn(src, fr.SrcMAC)
		}
		a.forwardDnat(fr.Data)
	case EtherTypeIPv6:
		a.handleIPv6(fr)
	default:
		// Unsupported ethertype (VLAN/QinQ/etc.) — ignore.
	}
}

func (a *Adapter) handleARP(fr Frame) {
	ap := ParseARP(fr.Data)
	if ap == nil {
		return
	}
	a.arp.Learn(ap.SenderIP, fr.SrcMAC)
	if ap.Op == ARPRequest {
		cfg, ok := a.eng.Current()
		if ok && cfg.Mac != 0 && hasAssigned4(cfg.Assigned, ap.TargetIP) {
			reply := BuildARPReply(cfg.Mac, ap.TargetIP, fr.SrcMAC, ap.SenderIP)
			_ = a.eng.SendFrame(Frame{Nwid: fr.Nwid, SrcMAC: cfg.Mac, DstMAC: fr.SrcMAC, EtherType: EtherTypeARP, Data: reply})
		}
	}
	// ARP reply (or gratuitous ARP): flush queued packets for the sender.
	a.flushPending(ap.SenderIP)
}

func (a *Adapter) handleIPv6(fr Frame) {
	src, ok := IPv6Src(fr.Data)
	if !ok {
		return
	}
	switch {
	case IsNeighborSolicitation(fr.Data):
		a.ndp.Learn(src, fr.SrcMAC)
		target := NSTarget(fr.Data)
		cfg, ok2 := a.eng.Current()
		if ok2 && cfg.Mac != 0 && target.IsValid() && hasAssigned6(cfg.Assigned, target) {
			na := BuildNA(cfg.Mac, target, target, cfg.Mac)
			_ = a.eng.SendFrame(Frame{Nwid: fr.Nwid, SrcMAC: cfg.Mac, DstMAC: fr.SrcMAC, EtherType: EtherTypeIPv6, Data: na})
		}
		// Consume: the Android TUN (L3) has no ZT addresses, so NS must be
		// answered here, not forwarded.
	case IsNeighborAdvertisement(fr.Data):
		a.ndp.Learn(src, fr.SrcMAC)
		target := NATarget(fr.Data)
		if target.IsValid() {
			if mac := LinkLayerAddrOption(fr.Data, 2); mac != 0 {
				a.ndp.Learn(target, mac)
			}
			a.flushPending(target)
		}
		// Consume.
	default:
		a.ndp.Learn(src, fr.SrcMAC)
		a.forward(fr.Data)
	}
}

func (a *Adapter) flushPending(ip netip.Addr) {
	a.pending.Flush(ip, func(pkt []byte) {
		dst, ok := PacketDest(pkt)
		if !ok {
			return
		}
		if err := a.SendIP(dst, pkt); err != nil {
			Warnf("[ZT] flush send to %s: %v", dst, err)
		}
	})
}

// snatOut rewrites the IPv4 source address of a TUN-bound packet to the
// ZT-assigned address before it enters the ZT network. The Android TUN
// subnet (172.19.0.0/30) is not routable inside ZT, so replies would have no
// return path. Idempotent: packets whose source is already the ZT IP pass
// through unchanged.
func (a *Adapter) snatOut(cfg Snapshot, pkt []byte) []byte {
	if !a.tunIPv4.IsValid() {
		return pkt
	}
	local := firstAssigned4(cfg.Assigned)
	if !local.IsValid() || local == a.tunIPv4 {
		return pkt
	}
	src, ok := IPv4Src(pkt)
	if !ok || src == local {
		return pkt
	}
	if rw := rewriteIPv4SrcDst(pkt, local, netip.Addr{}); rw != nil {
		Infof("[ZT] snat %s -> %s (TUN->ZT)", src, local)
		return rw
	}
	return pkt
}

// forwardDnat rewrites the IPv4 destination address of an inbound ZT packet
// from the ZT-assigned IP back to the TUN internal address before writing it
// to the TUN. Counterpart of snatOut.
func (a *Adapter) forwardDnat(pkt []byte) {
	if a.tunIPv4.IsValid() {
		if cfg, ok := a.eng.Current(); ok {
			if local := firstAssigned4(cfg.Assigned); local.IsValid() && local != a.tunIPv4 {
				if dst, ok := IPv4Dst(pkt); ok && dst == local {
					if rw := rewriteIPv4SrcDst(pkt, netip.Addr{}, a.tunIPv4); rw != nil {
						Infof("[ZT] dnat %s -> %s (ZT->TUN)", local, a.tunIPv4)
						a.forward(rw)
						return
					}
				}
			}
		}
	}
	a.forward(pkt)
}

func (a *Adapter) forward(pkt []byte) {
	if a.Out != nil {
		a.Out(pkt)
	}
}

// ---- helpers ----

func firstAssigned4(list []AssignedAddr) netip.Addr {
	for _, aa := range list {
		if aa.Addr.Is4() {
			return aa.Addr
		}
	}
	return netip.Addr{}
}

func firstAssigned6(list []AssignedAddr) netip.Addr {
	for _, aa := range list {
		if aa.Addr.Is6() {
			return aa.Addr
		}
	}
	return netip.Addr{}
}

func hasAssigned4(list []AssignedAddr, ip netip.Addr) bool {
	for _, aa := range list {
		if aa.Addr == ip {
			return true
		}
	}
	return false
}

// hasAssigned6 is an alias: address equality is family-agnostic.
func hasAssigned6(list []AssignedAddr, ip netip.Addr) bool {
	return hasAssigned4(list, ip)
}

func assignedBits(cfg Snapshot, ip netip.Addr) int {
	for _, aa := range cfg.Assigned {
		if aa.Addr == ip {
			return aa.Bits
		}
	}
	return 24 // fallback; caller only uses this for the gateway heuristic
}

// fragmentIPv4 splits an IPv4 packet into MTU-sized fragments (RFC 791).
// The original packet is copied for the first fragment (header kept); later
// fragments get a new header with offset set and MF flag, and carry a slice
// of the payload. The IP ID is preserved.
func fragmentIPv4(pkt []byte, mtu int) [][]byte {
	if len(pkt) < 20 {
		return [][]byte{pkt}
	}
	if mtu < 68 {
		mtu = 68
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || ihl > len(pkt) {
		return [][]byte{pkt}
	}
	payload := pkt[ihl:]
	id := uint16(pkt[4])<<8 | uint16(pkt[5])
	// mtu-aligned payload per fragment: 8-byte multiple
	plen := (mtu - ihl) / 8 * 8
	if plen <= 0 {
		plen = 8
	}
	var out [][]byte
	offset := 0
	for {
		off16 := offset / 8
		last := offset+plen >= len(payload)
		frag := make([]byte, ihl+min(plen, len(payload)-offset))
		copy(frag, pkt[:ihl])
		copy(frag[ihl:], payload[offset:offset+min(plen, len(payload)-offset)])
		// total length
		tl := len(frag)
		frag[2], frag[3] = byte(tl>>8), byte(tl)
		// flags+offset: DF=0, MF=1 unless last; offset
		fo := uint16(off16)
		if !last {
			fo |= 0x2000 // MF
		}
		frag[6], frag[7] = byte(fo>>8), byte(fo)
		// ID
		frag[4], frag[5] = byte(id>>8), byte(id)
		// RFC 791: header checksum MUST be recomputed after total length /
		// flags+offset changes (first fragment inherited the original
		// checksum from the copied header, which no longer matches).
		frag[10], frag[11] = 0, 0
		cs := ipv4HeaderChecksum(frag[:ihl])
		frag[10], frag[11] = byte(cs>>8), byte(cs&0xff)
		out = append(out, frag)
		if last {
			break
		}
		offset += plen
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
