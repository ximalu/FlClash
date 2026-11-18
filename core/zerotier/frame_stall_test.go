package zerotier

import (
	"testing"
	"time"
)

// 真实数据回归：2026-11-19 那份 16 小时 datapath 日志里，WiFi→蜂窝之后
// wire rx 每 10s 还在涨（+1~20 个 keepalive），而 frames rx / tun ok 冻住
// 持续 4 分钟。judgeSocketHealth 对这个窗口返回 ok（入站一直有），所以
// 必须由 judgeFrameProgress 把它判成 rebuild。
func TestJudgeFrameProgress(t *testing.T) {
	cases := []struct {
		name     string
		sampled  bool
		demand   uint64
		frozen   time.Duration
		rebuilds int
		fixGap   time.Duration
		want     frameVerdict
	}{
		{"还没采过样", false, 100, 0, 0, -1, frameHealthy},
		{"没有出站需求（熄屏）", true, 0, 5 * time.Minute, 0, -1, frameIdle},
		{"帧在前进", true, 100, 0, 0, -1, frameHealthy},
		{"冻 14s 不到阈值", true, 100, 14 * time.Second, 0, -1, frameHealthy},
		{"冻 15s 正好判死", true, 100, socketFrameStallTimeout, 0, -1, frameRebuildSocket},
		{"实机 4 分钟窗口", true, 2400, 4 * time.Minute, 0, -1, frameRebuildSocket},
		{"刚重建过，观察窗内", true, 100, 20 * time.Second, 1, 5 * time.Second, frameHealthy},
		{"重建间隔已过", true, 100, 40 * time.Second, 1, socketFrameStallMinGap, frameRebuildSocket},
		{"重建次数用尽 → 交给上层", true, 100, time.Minute, socketFrameStallMax, time.Minute, frameStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeFrameProgress(tc.sampled, tc.demand, tc.frozen, tc.rebuilds, tc.fixGap)
			if got != tc.want {
				t.Fatalf("judgeFrameProgress(%+v) = %v, want %v", tc, got, tc.want)
			}
		})
	}
}
