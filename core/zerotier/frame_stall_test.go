package zerotier

import (
	"testing"
	"time"
)

// 真实数据回归：2026-11-19 那份 16 小时 datapath 日志里，WiFi→蜂窝之后
// wire rx 每 10s 还在涨（+1~20 个 keepalive），而 frames rx / tun ok 冻住
// 持续 4 分钟 —— 原判据（零入站）对这个窗口返回 ok，必须由数据面判据抓住。
// 同时要保证「手机静置、没有 overlay 流量」的窗口不被误判。
func TestJudgeFrameProgress(t *testing.T) {
	cases := []struct {
		name       string
		framesSent uint64
		frozen     time.Duration
		rebuilds   int
		fixGap     time.Duration
		want       frameVerdict
	}{
		{"静置，没有 overlay 流量", 0, 10 * time.Minute, 0, -1, frameHealthy},
		{"只送了几帧就断（证据不足）", socketFrameStallMinFrames - 1, 5 * time.Minute, 0, -1, frameHealthy},
		{"送过帧，冻 19s 不到阈值", socketFrameStallMinFrames, 19 * time.Second, 0, -1, frameHealthy},
		{"送过帧，冻 20s 判死", socketFrameStallMinFrames, socketFrameStallTimeout, 0, -1, frameRebuildSocket},
		{"实机 4 分钟窗口", 2400, 4 * time.Minute, 0, -1, frameRebuildSocket},
		{"刚重建过，观察窗内", 100, 40 * time.Second, 1, 5 * time.Second, frameHealthy},
		{"重建间隔已过", 100, 60 * time.Second, 1, socketFrameStallMinGap, frameRebuildSocket},
		{"重建次数用尽 → 交给上层", 100, time.Minute, socketFrameStallMax, time.Minute, frameStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeFrameProgress(tc.framesSent, tc.frozen, tc.rebuilds, tc.fixGap)
			if got != tc.want {
				t.Fatalf("judgeFrameProgress(%+v) = %v, want %v", tc, got, tc.want)
			}
		})
	}
}
