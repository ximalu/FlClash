package zerotier

import (
	"testing"
	"time"
)

// 真实数据回归：2026-11-19 那份 16 小时 datapath 日志里，WiFi→蜂窝之后
// wire rx 每 10s 还在涨（+1~20 个 keepalive），而 frames rx / tun ok 冻住
// 持续 4 分钟 —— 原判据（零入站）对这个窗口返回 ok，必须由数据面判据抓住。
// 同时要保证「手机静置、没有 overlay 流量」的窗口不被误判。
//
// 升级链：20s 换 socket（最多 3 次、间隔 30s）→ 45s 换整个 ZT Node
// （最多 2 次、间隔 120s）→ 判 stalled 交给上层整包重启。
func TestJudgeFrameProgress(t *testing.T) {
	cases := []struct {
		name         string
		framesSent   uint64
		frozen       time.Duration
		rebuilds     int
		fixGap       time.Duration
		nodeRestarts int
		nodeGap      time.Duration
		want         frameVerdict
	}{
		{"静置，没有 overlay 流量", 0, 10 * time.Minute, 0, -1, 0, -1, frameHealthy},
		{"只送了几帧就断（证据不足）", socketFrameStallMinFrames - 1, 5 * time.Minute, 0, -1, 0, -1, frameHealthy},
		{"送过帧，冻 19s 不到阈值", socketFrameStallMinFrames, 19 * time.Second, 0, -1, 0, -1, frameHealthy},
		{"送过帧，冻 20s 判死", socketFrameStallMinFrames, socketFrameStallTimeout, 0, -1, 0, -1, frameRebuildSocket},
		{"实机 4 分钟窗口", 2400, 4 * time.Minute, 0, -1, 0, -1, frameRebuildSocket},
		{"刚重建过，观察窗内", 100, 40 * time.Second, 1, 5 * time.Second, 0, -1, frameHealthy},
		{"重建间隔已过", 100, 60 * time.Second, 1, socketFrameStallMinGap, 0, -1, frameRebuildSocket},
		// socket 档位用尽：先给 node 重建一个观察期，冻得不够久不升级。
		{"socket 用尽但冻得不够久", 100, socketFrameNodeRestartFrozenFor - time.Second, socketFrameStallMax, time.Minute, 0, -1, frameHealthy},
		{"socket 用尽 + 冻 45s → 重建 node", 100, socketFrameNodeRestartFrozenFor, socketFrameStallMax, time.Minute, 0, -1, frameRestartNode},
		// socket 观察窗内但已经冻很久：不能干等 socket 间隔，直接升级。
		{"socket 观察窗内 + 冻 45s → 重建 node", 100, socketFrameNodeRestartFrozenFor, 1, 5 * time.Second, 0, -1, frameRestartNode},
		{"node 刚重建过，观察窗内", 100, 90 * time.Second, socketFrameStallMax, 5 * time.Minute, 1, 30 * time.Second, frameHealthy},
		{"node 间隔已过", 100, 90 * time.Second, socketFrameStallMax, 5 * time.Minute, 1, socketFrameNodeRestartMinGap, frameRestartNode},
		{"node 重建用尽 → 交给上层", 100, 5 * time.Minute, socketFrameStallMax, 5 * time.Minute, socketFrameNodeRestartMax, 5 * time.Minute, frameStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeFrameProgress(tc.framesSent, tc.frozen, tc.rebuilds, tc.fixGap, tc.nodeRestarts, tc.nodeGap)
			if got != tc.want {
				t.Fatalf("judgeFrameProgress(%+v) = %v, want %v", tc, got, tc.want)
			}
		})
	}
}
