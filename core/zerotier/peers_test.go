//go:build cgo

package zerotier

import (
	"errors"
	"testing"
	"time"
)

// TestJudgeSocketHealth pins every branch of the single link-health
// judgement. Thresholds come from the watchdog constants;
// socketDeadTimeout=60s, socketFixMinGap=60s, socketFixMax=10.
func TestJudgeSocketHealth(t *testing.T) {
	cases := []struct {
		name     string
		rebuild  bool
		sampled  bool
		silent   time.Duration
		tx       uint64
		fixes    int
		fixGap   time.Duration
		want     string
	}{
		{name: "rebuilding_wins", rebuild: true, sampled: true, silent: 5 * time.Second, tx: 3, want: healthRebuilding},
		{name: "not_sampled_yet", sampled: false, want: healthUnknown},
		{name: "no_outbound_is_idle", sampled: true, silent: 10 * time.Minute, tx: 0, want: healthIdle},
		{name: "fresh_inbound_ok", sampled: true, silent: 3 * time.Second, tx: 12, want: healthOK},
		{name: "dead_before_timeout_is_not_stalled", sampled: true, silent: 59 * time.Second, tx: 40, want: healthOK},
		{name: "dead_after_timeout_can_heal", sampled: true, silent: 61 * time.Second, tx: 40, fixes: 0, fixGap: -1, want: healthRebuilding},
		{name: "dead_gap_not_elapsed", sampled: true, silent: 90 * time.Second, tx: 40, fixes: 1, fixGap: 30 * time.Second, want: healthStalled},
		{name: "dead_fix_max_reached", sampled: true, silent: 90 * time.Second, tx: 40, fixes: socketFixMax, fixGap: 2 * time.Minute, want: healthStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeSocketHealth(tc.rebuild, tc.sampled, tc.silent, tc.tx, tc.fixes, tc.fixGap)
			if got != tc.want {
				t.Fatalf("judgeSocketHealth(%+v) = %q, want %q", tc, got, tc.want)
			}
		})
	}
}

// TestLinkStatsUnknownBeforeSampling: a fresh Engine has no watchdog sample,
// so silent/txSinceRx must be nil (unknown) rather than 0.
func TestLinkStatsUnknownBeforeSampling(t *testing.T) {
	e := &Engine{}
	stats := e.linkStats(time.Now())
	if stats.Health != healthUnknown {
		t.Fatalf("health = %q, want %q", stats.Health, healthUnknown)
	}
	if stats.SilentMs != nil || stats.WireTxSinceRx != nil {
		t.Fatalf("expected nil silent/txSinceRx, got %+v", stats)
	}
}

// TestLinkStatsReportsSampledValues: once the watchdog has sampled, the
// status carries the numbers (and the sampled verdict) verbatim.
func TestLinkStatsReportsSampledValues(t *testing.T) {
	e := &Engine{}
	now := time.Now()
	e.wdMu.Lock()
	e.wdLastRxAt = now.Add(-70 * time.Second)
	e.wdTxAtRx = 0
	e.wdHealth = healthStalled
	e.wdMu.Unlock()

	stats := e.linkStats(now)
	if stats.Health != healthStalled {
		t.Fatalf("health = %q, want %q", stats.Health, healthStalled)
	}
	if stats.SilentMs == nil || *stats.SilentMs < 69000 || *stats.SilentMs > 71000 {
		t.Fatalf("silentForMs = %v, want ~70000", stats.SilentMs)
	}
	if stats.WireTxSinceRx == nil {
		t.Fatal("wireTxSinceRx must be set once sampled")
	}
}

func TestFormatPeerVersion(t *testing.T) {
	if got := formatPeerVersion(-1, -1, -1); got != "" {
		t.Fatalf("unknown version = %q, want empty", got)
	}
	if got := formatPeerVersion(1, 16, 2); got != "1.16.2" {
		t.Fatalf("version = %q, want 1.16.2", got)
	}
}

func TestPeerRoleString(t *testing.T) {
	for role, want := range map[int32]string{0: "LEAF", 1: "MOON", 2: "PLANET", 7: ""} {
		if got := peerRoleString(role); got != want {
			t.Fatalf("peerRoleString(%d) = %q, want %q", role, got, want)
		}
	}
}

// TestPeerStatusesWithoutEngine: no node → ok=false (unknown), never an empty
// list that the UI would read as "0 peers".
func TestPeerStatusesWithoutEngine(t *testing.T) {
	withNoEngine(t)
	e := &Engine{}
	if snap := e.PeerSnapshot(); snap.Known || snap.Peers != nil {
		t.Fatalf("PeerSnapshot without node = %+v, want unknown/empty", snap)
	}
	status := GetRuntimeStatus()
	if status.Peers != nil {
		t.Fatalf("status.Peers = %v, want nil (unknown)", status.Peers)
	}
	if status.LinkHealth != healthUnknown {
		t.Fatalf("linkHealth = %q, want %q", status.LinkHealth, healthUnknown)
	}
	if status.UpdatedAt == 0 {
		t.Fatal("updatedAt must always be set")
	}
}

// TestStartErrorLifecycle: the fatal-start-error channel is written on a
// failed start and cleared on success/stop; it survives the engine being gone.
func TestStartErrorLifecycle(t *testing.T) {
	withNoEngine(t)
	clearStartError()
	defer clearStartError()

	recordStartError(errors.New("bind UDP port 9994: address already in use"))
	status := GetRuntimeStatus()
	if status.LastError == "" || status.LastErrorAt == 0 {
		t.Fatalf("lastError not surfaced: %+v", status)
	}

	clearStartError()
	if status := GetRuntimeStatus(); status.LastError != "" {
		t.Fatalf("lastError = %q, want empty after clear", status.LastError)
	}
}

// withNoEngine hides any global engine for the duration of a test.
func withNoEngine(t *testing.T) {
	t.Helper()
	globalEngineMu.Lock()
	oldEngine := globalEngine
	oldDone := globalDone
	globalEngine = nil
	globalDone = nil
	globalEngineMu.Unlock()
	t.Cleanup(func() {
		globalEngineMu.Lock()
		globalEngine = oldEngine
		globalDone = oldDone
		globalEngineMu.Unlock()
	})
}
