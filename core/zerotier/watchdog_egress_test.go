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

// 只复位「入向还活着」的 LEAF：这正是单向断链的特征。
func TestEgressTargetPeers(t *testing.T) {
	now := int64(1_000_000)
	peers := []PeerStatus{
		{Address: "a5cd13296e", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 3_000},      // ✓ 刚收到
		{Address: "8efcdece80", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 40_000},     // ✗ 太久没收到
		{Address: "778cde7190", Role: "PLANET", PathCount: 1, LastReceiveAtMs: now - 1_000},    // ✗ root 不动
		{Address: "b6079f73c6", Role: "LEAF", PathCount: 0, LastReceiveAtMs: now - 1_000},      // ✗ 没路径
		{Address: "cafe04eba9", Role: "MOON", PathCount: 1, LastReceiveAtMs: now - 1_000},      // ✗ 非 LEAF
	}
	got := egressTargetPeers(peers, now)
	if len(got) != 1 || got[0].Address != "a5cd13296e" {
		t.Fatalf("targets = %+v, want only a5cd13296e", got)
	}
}

// 双向全哑（没有任何 peer 还在发给我们）：不该乱复位，交给 socket 看门狗。
func TestEgressTargetPeersAllSilent(t *testing.T) {
	now := int64(1_000_000)
	peers := []PeerStatus{
		{Address: "a5cd13296e", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 120_000},
	}
	if got := egressTargetPeers(peers, now); len(got) != 0 {
		t.Fatalf("targets = %+v, want none", got)
	}
}
