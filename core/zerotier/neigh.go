package zerotier

import (
	"net/netip"
	"sync"
	"time"
)

// NeighTable is a MAC resolution cache (IPv4 ARP / IPv6 NDP), modeled after
// the official ZeroTier Android TunTapAdapter (120s entry timeout).
type NeighTable struct {
	mu      sync.RWMutex
	entries map[netip.Addr]neighEntry
	ttl     time.Duration
}

type neighEntry struct {
	mac    uint64
	expire time.Time
}

// NewNeighTable returns a table whose entries expire after ttl.
func NewNeighTable(ttl time.Duration) *NeighTable {
	return &NeighTable{
		entries: make(map[netip.Addr]neighEntry),
		ttl:     ttl,
	}
}

// Learn records ip -> mac.
func (t *NeighTable) Learn(ip netip.Addr, mac uint64) {
	if !ip.IsValid() || mac == 0 {
		return
	}
	t.mu.Lock()
	t.entries[ip] = neighEntry{mac: mac, expire: time.Now().Add(t.ttl)}
	t.mu.Unlock()
}

// Get returns the MAC for ip, or 0 if unknown/expired.
func (t *NeighTable) Get(ip netip.Addr) uint64 {
	t.mu.RLock()
	e, ok := t.entries[ip]
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	if time.Now().After(e.expire) {
		t.Remove(ip)
		return 0
	}
	return e.mac
}

// Remove deletes an entry.
func (t *NeighTable) Remove(ip netip.Addr) {
	t.mu.Lock()
	delete(t.entries, ip)
	t.mu.Unlock()
}

// Cleanup drops expired entries; returns the number removed.
func (t *NeighTable) Cleanup(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for ip, e := range t.entries {
		if now.After(e.expire) {
			delete(t.entries, ip)
			n++
		}
	}
	return n
}

// Len returns the number of live entries.
func (t *NeighTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// PendingQueue holds IP packets awaiting MAC resolution (ARP/NDP).
// On resolution the packets are flushed in order; expired entries are
// dropped (the OS TCP/IP stack retransmits).
type PendingQueue struct {
	mu sync.Mutex
	q  map[netip.Addr][]pendingEntry
}

type pendingEntry struct {
	pkt    []byte
	expire time.Time
}

// NewPendingQueue returns an empty queue.
func NewPendingQueue() *PendingQueue {
	return &PendingQueue{q: make(map[netip.Addr][]pendingEntry)}
}

// Add queues pkt for ip until the given time.
func (p *PendingQueue) Add(ip netip.Addr, pkt []byte, until time.Time) {
	p.mu.Lock()
	p.q[ip] = append(p.q[ip], pendingEntry{pkt: append([]byte(nil), pkt...), expire: until})
	p.mu.Unlock()
}

// Flush delivers all queued packets for ip to send (in order) and removes
// them. send is called without the queue lock.
func (p *PendingQueue) Flush(ip netip.Addr, send func(pkt []byte)) {
	p.mu.Lock()
	entries := p.q[ip]
	delete(p.q, ip)
	p.mu.Unlock()
	for _, e := range entries {
		send(e.pkt)
	}
}

// Cleanup drops expired entries; returns the number dropped.
func (p *PendingQueue) Cleanup(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for ip, list := range p.q {
		kept := list[:0]
		for _, e := range list {
			if now.Before(e.expire) {
				kept = append(kept, e)
			} else {
				n++
			}
		}
		if len(kept) == 0 {
			delete(p.q, ip)
		} else {
			p.q[ip] = kept
		}
	}
	return n
}

// Len returns the total number of queued packets.
func (p *PendingQueue) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, l := range p.q {
		n += len(l)
	}
	return n
}
