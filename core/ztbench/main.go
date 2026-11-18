//go:build linux && !android && cgo

// ztbench drives the REAL FlClashTier ZeroTier data path on a Linux box:
// embedded core -> adapter (SNAT + fragmentation + frame queues) -> physical
// UDP wire -> a UDP sink on a ZeroTier peer. No Android device involved.
//
// Purpose: give a headless, repeatable throughput number so datapath changes
// can be compared before/after commit by commit.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"sync/atomic"
	"time"

	"core/zerotier"
)

var (
	sentPkts  atomic.Uint64
	sentBytes atomic.Uint64
	sendErrs  atomic.Uint64
	recvPkts  atomic.Uint64
	recvBytes atomic.Uint64
)

func main() {
	nwid := flag.String("nwid", "b6079f73c6c0eb31", "ZeroTier network id")
	home := flag.String("home", "/tmp/ztbench-home", "home dir holding zerotier-identity.secret")
	target := flag.String("target", "192.168.196.126:9999", "peer ZT ip:port of the UDP sink")
	dur := flag.Duration("dur", 30*time.Second, "test duration")
	size := flag.Int("size", 9000, "IP packet size in bytes (9000 forces fragmentation at ZT MTU 2800)")
	pps := flag.Int("pps", 0, "max packets/sec (0 = unthrottled)")
	wirePort := flag.Int("port", zerotier.DefaultPort, "ZT wire UDP port (use a unique port so tc filters can isolate benchmark traffic)")
	sport := flag.Int("sport", 40000, "UDP source port (tag each run so the sink can attribute)")
	logPath := flag.String("log", "", "emulate device logging: write ZT info logs to this file")
	flag.Parse()

	if err := os.MkdirAll(*home, 0o700); err != nil {
		fmt.Println("mkdir:", err)
		os.Exit(1)
	}

	if *logPath != "" {
		// Emulate the Android setup where mihomo's logger is installed: the
		// per-packet Infof calls in the OLD code then cost real I/O per frame.
		f, err := os.Create(*logPath)
		if err != nil {
			fmt.Println("log file:", err)
			os.Exit(1)
		}
		zerotier.Logger = func(level, format string, args ...any) {
			fmt.Fprintf(f, level+" "+format+"\n", args...)
		}
	}

	dstAddr, err := netip.ParseAddrPort(*target)
	if err != nil {
		fmt.Println("target:", err)
		os.Exit(1)
	}

	cfg := zerotier.Config{NetworkID: *nwid, Port: *wirePort}
	eng, err := zerotier.StartEngine(cfg, nil, *home)
	if err != nil {
		fmt.Println("StartEngine:", err)
		os.Exit(1)
	}
	defer eng.Stop()

	// The Android app's TUN address; SNAT rewrites it to the ZT-assigned IP.
	adapter := zerotier.NewAdapter(eng)
	adapter.SetTUNAddress(netip.MustParseAddr("172.19.0.1"))
	adapter.Out = func(pkt []byte) {
		recvPkts.Add(1)
		recvBytes.Add(uint64(len(pkt)))
	}

	go func() {
		for fr := range eng.Frames() {
			adapter.HandleFrame(fr)
		}
	}()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for now := range t.C {
			adapter.Cleanup(now)
		}
	}()

	st := zerotier.GetRuntimeStatus()
	fmt.Printf("node address : %s\n", st.NodeAddress)
	fmt.Printf("target       : %s\n", dstAddr.String())
	fmt.Printf("packet size  : %d bytes\n", *size)

	// Wait for the ZT config (assigned IP + managed routes).
	deadline := time.Now().Add(90 * time.Second)
	lastDiag := time.Now()
	for time.Now().Before(deadline) {
		st = zerotier.GetRuntimeStatus()
		if st.IPv4 != "" && st.Routes > 0 {
			break
		}
		if time.Since(lastDiag) >= 5*time.Second {
			fd, ok, ea, en, ot := eng.DataPathCounters()
			fmt.Printf("  waiting: ip=%q routes=%d | wire ok=%d eagain=%d enobufs=%d other=%d | qdrop C=%d Go=%d\n",
				st.IPv4, st.Routes, ok, ea, en, ot, fd, eng.FramesDropped())
			lastDiag = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
	st = zerotier.GetRuntimeStatus()
	if st.IPv4 == "" {
		fmt.Println("FAILED: node has no assigned IP after 90s — is this identity authorized in ZeroTier Central?")
		os.Exit(2)
	}
	if snap, ok := eng.Current(); ok {
		fmt.Printf("assigned ip  : %s (routes=%d)  ZT network MTU=%d", st.IPv4, st.Routes, snap.MTU)
		for _, r := range snap.Routes {
			fmt.Printf("  route %s", r.Prefix.String())
		}
		fmt.Println()
	} else {
		fmt.Printf("assigned ip  : %s (routes=%d)\n", st.IPv4, st.Routes)
	}
	rcv, snd := zerotier.UDPSocketBuffers()
	fmt.Printf("udp buffers  : rcv=%d snd=%d\n", rcv, snd)
	fmt.Println("--- sending ---")

	pkt := buildPacket(netip.MustParseAddr("172.19.0.1"), dstAddr.Addr(), uint16(*sport), uint16(dstAddr.Port()), *size)
	start := time.Now()
	lastReport := start
	var lastSent uint64
	var lastRecv uint64

	for time.Since(start) < *dur {
		if err := adapter.SendIP(dstAddr.Addr(), pkt); err != nil {
			sendErrs.Add(1)
		} else {
			sentPkts.Add(1)
			sentBytes.Add(uint64(len(pkt)))
		}
		if *pps > 0 {
			time.Sleep(time.Second / time.Duration(*pps))
		}
		if time.Since(lastReport) >= 5*time.Second {
			dt := time.Since(lastReport).Seconds()
			s := sentBytes.Load()
			r := recvBytes.Load()
			fmt.Printf("  %5.1fs  sent %.1f MB (%.0f KB/s)  recv %.1f MB (%.0f KB/s)  err=%d\n",
				time.Since(start).Seconds(), float64(s)/1e6, float64(s-lastSent)/1024/dt,
				float64(r)/1e6, float64(r-lastRecv)/1024/dt, sendErrs.Load())
			lastSent, lastRecv = s, r
			lastReport = time.Now()
		}
	}

	elapsed := time.Since(start).Seconds()
	ast := adapter.Stats()
	fd, ok, ea, en, ot := eng.DataPathCounters()
	fmt.Println("--- result ---")
	fmt.Printf("duration          : %.1fs\n", elapsed)
	fmt.Printf("sent              : %d pkts / %.2f MB  (%.0f KB/s)\n",
		sentPkts.Load(), float64(sentBytes.Load())/1e6, float64(sentBytes.Load())/1024/elapsed)
	fmt.Printf("recv (loopback)   : %d pkts / %.2f MB\n", recvPkts.Load(), float64(recvBytes.Load())/1e6)
	fmt.Printf("send errors       : %d\n", sendErrs.Load())
	fmt.Printf("adapter           : framesRx=%d snat=%d dnat=%d frag4=%d drop6=%d\n",
		ast.FramesRx, ast.Snat, ast.Dnat, ast.Fragmented4, ast.Dropped6)
	fmt.Printf("queue drops       : C-ring=%d go-chan=%d (capacity %d)\n", fd, eng.FramesDropped(), zerotier.FrameQueueCapacity())
	fmt.Printf("wire send         : ok=%d eagain=%d enobufs=%d other=%d\n", ok, ea, en, ot)
}

// buildPacket builds an IPv4+UDP packet (checksum 0 = "not computed", legal on
// IPv4) of the requested total size, with src 172.19.0.1 so the adapter's SNAT
// path is exercised exactly like on Android.
func buildPacket(src, dst netip.Addr, sport, dport uint16, size int) []byte {
	if size < 28 {
		size = 28
	}
	pkt := make([]byte, size)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(size))
	pkt[8] = 64
	pkt[9] = 17 // UDP
	sb, db := src.As4(), dst.As4()
	copy(pkt[12:16], sb[:])
	copy(pkt[16:20], db[:])
	binary.BigEndian.PutUint16(pkt[20:22], sport)
	binary.BigEndian.PutUint16(pkt[22:24], dport)
	binary.BigEndian.PutUint16(pkt[24:26], uint16(size-20))
	for i := 28; i < size; i++ {
		pkt[i] = byte(i)
	}

	// Real UDP checksum (pseudo-header + UDP header + payload). A zero checksum
	// is legal on IPv4 but would hide exactly the bug under test, and real
	// Android traffic always carries a computed checksum.
	binary.BigEndian.PutUint16(pkt[26:28], 0)
	var usum uint32
	usum += uint32(binary.BigEndian.Uint16(pkt[12:14]))
	usum += uint32(binary.BigEndian.Uint16(pkt[14:16]))
	usum += uint32(binary.BigEndian.Uint16(pkt[16:18]))
	usum += uint32(binary.BigEndian.Uint16(pkt[18:20]))
	usum += 17
	usum += uint32(size - 20)
	for i := 20; i+1 < size; i += 2 {
		usum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	if size%2 == 1 {
		usum += uint32(pkt[size-1]) << 8
	}
	for usum > 0xffff {
		usum = (usum >> 16) + (usum & 0xffff)
	}
	ucs := ^uint16(usum)
	if ucs == 0 {
		ucs = 0xffff
	}
	binary.BigEndian.PutUint16(pkt[26:28], ucs)

	// IPv4 header checksum.
	pkt[10], pkt[11] = 0, 0
	var isum uint32
	for i := 0; i < 20; i += 2 {
		isum += uint32(binary.BigEndian.Uint16(pkt[i : i+2]))
	}
	for isum > 0xffff {
		isum = (isum >> 16) + (isum & 0xffff)
	}
	binary.BigEndian.PutUint16(pkt[10:12], ^uint16(isum))
	return pkt
}
