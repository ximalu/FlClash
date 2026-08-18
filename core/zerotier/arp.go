package zerotier

import "net/netip"

// ARP packet (RFC 826), 28 bytes (no padding):
//
//	HTYPE(2) PTYPE(2) HLEN(1) PLEN(1) OPER(2) SHA(6) SPA(4) THA(6) TPA(4)
const (
	ARPRequest = 1
	ARPReply   = 2
)

// ARPPacket is a parsed IPv4-over-Ethernet ARP message.
type ARPPacket struct {
	Op        uint16
	SenderMAC uint64
	SenderIP  netip.Addr
	TargetMAC uint64
	TargetIP  netip.Addr
}

// ParseARP validates and parses a 28-byte ARP payload.
func ParseARP(data []byte) *ARPPacket {
	if len(data) < 28 ||
		data[0] != 0 || data[1] != 1 || // Ethernet
		data[2] != 8 || data[3] != 0 || // IPv4
		data[4] != 6 || data[5] != 4 { // hlen=6 plen=4
		return nil
	}
	return &ARPPacket{
		Op:        uint16(data[6])<<8 | uint16(data[7]),
		SenderMAC: MacFromBytes(data[8:14]),
		SenderIP:  addr4(data[14:18]),
		TargetMAC: MacFromBytes(data[18:24]),
		TargetIP:  addr4(data[24:28]),
	}
}

// BuildARPRequest builds "who has targetIP?" from localMAC/localIP.
func BuildARPRequest(localMAC uint64, localIP, targetIP netip.Addr) []byte {
	return buildARP(ARPRequest, localMAC, localIP, 0, targetIP)
}

// BuildARPReply builds "targetIP is at localMAC" for the given requester.
func BuildARPReply(localMAC uint64, localIP netip.Addr, targetMAC uint64, targetIP netip.Addr) []byte {
	return buildARP(ARPReply, localMAC, localIP, targetMAC, targetIP)
}

func buildARP(op uint16, srcMAC uint64, srcIP netip.Addr, dstMAC uint64, dstIP netip.Addr) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 0, 1 // Ethernet
	b[2], b[3] = 8, 0 // IPv4
	b[4] = 6          // hlen
	b[5] = 4          // plen
	b[6], b[7] = byte(op>>8), byte(op)
	MacToBytes(srcMAC, b[8:14])
	s4 := srcIP.As4()
	copy(b[14:18], s4[:])
	MacToBytes(dstMAC, b[18:24])
	d4 := dstIP.As4()
	copy(b[24:28], d4[:])
	return b
}
