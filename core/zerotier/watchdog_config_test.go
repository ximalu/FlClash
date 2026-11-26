//go:build cgo

package zerotier

import (
	"testing"
	"time"
)

func TestJudgeConfigStall(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		stalled  time.Duration
		txDelta  uint64
		nudges   int
		nudgeGap time.Duration
		want     configVerdict
	}{
		{"config OK is healthy", statusOK, time.Hour, 100, 0, -1, configHealthy},
		{"not stalled long enough", 0, 30 * time.Second, 100, 0, -1, configHealthy},
		{"no wire tx = insufficient evidence", 0, 5 * time.Minute, 5, 0, -1, configHealthy},
		{"stalled with traffic -> nudge", 0, 60 * time.Second, 100, 0, -1, configNudge},
		{"just nudged, within gap", 0, 60 * time.Second, 100, 1, 10 * time.Second, configHealthy},
		{"second nudge after gap", 0, 2 * time.Minute, 200, 1, 90 * time.Second, configNudge},
		{"nudges exhausted -> give up", 0, 5 * time.Minute, 500, configStallMaxNudges, 90 * time.Second, configExhausted},
		{"access denied still nudges (controller may be stale)", 2, 60 * time.Second, 100, 0, -1, configNudge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeConfigStall(tc.status, tc.stalled, tc.txDelta, tc.nudges, tc.nudgeGap)
			if got != tc.want {
				t.Fatalf("judgeConfigStall() = %v, want %v", got, tc.want)
			}
		})
	}
}
