package zerotier

import (
	"encoding/binary"
	"testing"
)

// buildSYN 构造一个带 MSS option 的 IPv4 TCP SYN 包。
func buildSYN(mss uint16, mtu int, flags byte) []byte {
	// IP(20) + TCP(20+4 for MSS option)
	pkt := make([]byte, 20+24)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 6 // TCP
	copy(pkt[12:16], []byte{172, 19, 0, 1})
	copy(pkt[16:20], []byte{192, 168, 196, 126})
	binary.BigEndian.PutUint16(pkt[10:12], ipv4HeaderChecksum(pkt[:20]))

	tcp := pkt[20:]
	binary.BigEndian.PutUint16(tcp[0:2], 40000)
	binary.BigEndian.PutUint16(tcp[2:4], 443)
	tcp[12] = 6 << 4 // data offset = 24 bytes
	tcp[13] = flags
	// MSS option: kind=2 len=4 value
	tcp[20] = 2
	tcp[21] = 4
	binary.BigEndian.PutUint16(tcp[22:24], mss)
	putTCPChecksum(pkt)
	return pkt
}

// tcpChecksum 全量重算 TCP 校验和（把校验和字段当 0）。
func recomputeTCPChecksum(pkt []byte) uint16 {
	ihl := int(pkt[0]&0x0f) * 4
	seg := pkt[ihl:]
	var sum uint32
	// pseudo header
	sum += uint32(binary.BigEndian.Uint16(pkt[12:14])) + uint32(binary.BigEndian.Uint16(pkt[14:16]))
	sum += uint32(binary.BigEndian.Uint16(pkt[16:18])) + uint32(binary.BigEndian.Uint16(pkt[18:20]))
	sum += 6
	sum += uint32(len(seg))
	for i := 0; i+1 < len(seg); i += 2 {
		if i == 16 { // 跳过校验和字段
			continue
		}
		sum += uint32(binary.BigEndian.Uint16(seg[i : i+2]))
	}
	if len(seg)%2 == 1 {
		sum += uint32(seg[len(seg)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func putTCPChecksum(pkt []byte) {
	ihl := int(pkt[0]&0x0f) * 4
	pkt[ihl+16], pkt[ihl+17] = 0, 0
	c := recomputeTCPChecksum(pkt)
	pkt[ihl+16], pkt[ihl+17] = byte(c>>8), byte(c)
}

func mssOf(pkt []byte) uint16 {
	tcp := pkt[20:]
	return binary.BigEndian.Uint16(tcp[22:24])
}

func TestClampTCPMSSRewritesSYNAndKeepsChecksumValid(t *testing.T) {
	pkt := buildSYN(8960, 9000, 0x02) // SYN, 宣告 8960
	want := mssTargetFor(2800)
	if want != 2756 {
		t.Fatalf("mssTargetFor(2800) = %d, want 2756", want)
	}
	if !clampTCPMSS(pkt, want) {
		t.Fatal("expected MSS to be clamped")
	}
	if got := mssOf(pkt); got != want {
		t.Fatalf("MSS = %d, want %d", got, want)
	}
	// 校验和必须仍然正确（否则对端会丢掉这个 SYN，反而更糟）
	ihl := int(pkt[0]&0x0f) * 4
	stored := binary.BigEndian.Uint16(pkt[ihl+16 : ihl+18])
	if rec := recomputeTCPChecksum(pkt); rec != stored {
		t.Fatalf("TCP checksum broken: stored=%#x recomputed=%#x", stored, rec)
	}
}

func TestClampTCPMSSLeavesSmallMSSAlone(t *testing.T) {
	pkt := buildSYN(1400, 9000, 0x02)
	before := append([]byte(nil), pkt...)
	if clampTCPMSS(pkt, mssTargetFor(2800)) {
		t.Fatal("small MSS must not be touched")
	}
	if string(pkt) != string(before) {
		t.Fatal("packet changed")
	}
}

func TestClampTCPMSSIgnoresNonSYN(t *testing.T) {
	pkt := buildSYN(8960, 9000, 0x10) // ACK only
	if clampTCPMSS(pkt, 2756) {
		t.Fatal("non-SYN must not be touched")
	}
}

func TestClampTCPMSSIgnoresNonTCP(t *testing.T) {
	pkt := buildSYN(8960, 9000, 0x02)
	pkt[9] = 17 // UDP
	if clampTCPMSS(pkt, 2756) {
		t.Fatal("non-TCP must not be touched")
	}
}

func TestClampTCPMSSHandlesBadOptionsSafely(t *testing.T) {
	pkt := buildSYN(8960, 9000, 0x02)
	pkt[20+21] = 99 // MSS option length 非法
	if clampTCPMSS(pkt, 2756) {
		t.Fatal("malformed options must be ignored")
	}
}
