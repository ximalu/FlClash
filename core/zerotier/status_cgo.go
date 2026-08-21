//go:build cgo

package zerotier

// GetRuntimeStatus returns the current ZeroTier runtime state from the live
// in-memory Engine. No persisted file or heartbeat participates in liveness.
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
	const hex = "0123456789abcdef"
	var buf [16]byte
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = hex[address&0xf]
		address >>= 4
	}
	return string(buf[:])
}
