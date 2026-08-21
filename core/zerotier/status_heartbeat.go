package zerotier

import "time"

// statusHeartbeatLoop keeps zerotier-status.json a real runtime heartbeat.
//
// The status file is persistent across Android reboots, so a single RUNNING
// record cannot by itself mean that the Engine is still alive. The existing
// engine writes the file when its config snapshot changes; that is not frequent
// enough to serve as a liveness signal. This small package-level loop refreshes
// the file only while an Engine is actually RUNNING.
//
// Kept in a separate file deliberately: this is UI observability plumbing and
// does not alter the ZeroTier data plane or Engine lifecycle code.
func init() {
	go statusHeartbeatLoop()
}

func statusHeartbeatLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		e := currentEngine()
		if e == nil || e.getState() != StateRunning {
			continue
		}
		e.writeStatus("RUNNING", "")
	}
}
