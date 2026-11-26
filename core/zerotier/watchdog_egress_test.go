//go:build cgo

package zerotier

import (
	"testing"
	"time"
)

func TestJudgeEgressProgress(t *testing.T) {
	cases := []struct {
		name      string
		snat      uint64
		dnat      uint64
		frozen    time.Duration
		resets    int
		resetGap  time.Duration
		want      egressVerdict
	}{
		{"dnat advancing means alive", 100, 1, 5 * time.Minute, 0, -1, egressHealthy},
		{"not enough outbound demand", 3, 0, 5 * time.Minute, 0, -1, egressHealthy},
		{"frozen below timeout", 50, 0, 10 * time.Second, 0, -1, egressHealthy},
		{"dead outbound -> reset once", 50, 0, 21 * time.Second, 0, -1, egressResetPaths},
		{"just reset, still in window", 50, 0, 25 * time.Second, 1, 5 * time.Second, egressHealthy},
		{"second reset allowed after gap", 50, 0, 60 * time.Second, 1, 40 * time.Second, egressResetPaths},
		{"resets exhausted -> stalled", 50, 0, 60 * time.Second, egressResetMax, 40 * time.Second, egressStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeEgressProgress(tc.snat, tc.dnat, tc.frozen, tc.resets, tc.resetGap)
			if got != tc.want {
				t.Fatalf("judgeEgressProgress() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 静置手机（没有出站需求）时不能判死：snat 不涨就是证据不足。
func TestJudgeEgressProgressIdleDeviceStaysHealthy(t *testing.T) {
	if got := judgeEgressProgress(0, 0, 10*time.Minute, 0, -1); got != egressHealthy {
		t.Fatalf("idle device judged %v, want healthy", got)
	}
}
