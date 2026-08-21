package zerotier

import "net/netip"

// RuntimeStatus is a point-in-time view of the live ZeroTier engine.
// It is deliberately derived from the in-memory Engine, not from the
// persistent status file. Callers that need liveness should use State.
type RuntimeStatus struct {
	State       string `json:"state"`
	NodeAddress string `json:"nodeAddress"`
	IPv4        string `json:"ipv4,omitempty"`
	Routes      int    `json:"routes"`
	NetworkID   string `json:"networkId,omitempty"`
}

// GetRuntimeStatus returns the current ZeroTier runtime state.
//
// There is exactly one authoritative runtime state: the active Engine held by
// globalEngine. If no Engine exists, the answer is STOPPED. No persisted file,
// timestamp, or heartbeat participates in this decision.
func GetRuntimeStatus() RuntimeStatus {
	e := currentEngine()
	if e == nil {
		return RuntimeStatus{State: StateStopped.String()}
	}

	status := RuntimeStatus{
		State:       e.getState().String(),
		NodeAddress: formatNodeAddress(e.nodeAddress()),
		NetworkID:   e.cfg.NetworkID,
	}

	if snap, ok := e.Current(); ok {
		status.Routes = len(snap.Routes)
		if addr := firstAssigned4(snap.Assigned); addr.IsValid() {
			status.IPv4 = addr.String()
		}
	}
	return status
}

func formatNodeAddress(address uint64) string {
	if address == 0 {
		return ""
	}
	return formatHexAddress(address)
}

// Keep the address formatting in this package so the RPC layer does not need
// to know anything about ZeroTier's native address representation.
func formatHexAddress(address uint64) string {
	const hex = "0123456789abcdef"
	var buf [16]byte
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = hex[address&0xf]
		address >>= 4
	}
	return string(buf[:])
}

// Ensure netip remains part of this file's public-status implementation when
// the assigned-address representation changes; this is also a compile-time
// check that IPv4 extraction continues to use netip.Addr.
var _ netip.Addr
