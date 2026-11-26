//go:build linux && !android && cgo

package zerotier

// 换网路径自愈的回归防线（0.9.20）：rebind 之后先做软探测爆发（不清 lr、不丢端点），
// 只有爆发用尽且帧仍未前进才升级硬 reset。这里把节奏压到毫秒级直接观测计数。
//
// 背景（issues-backlog #10）：硬 reset 会把 lr 清零，下一轮 doPingAndKeepalive 直接
// 删掉 path 对象、端点地址一起忘掉；端口限制型 NAT 上就再没有第二次机会，只能等
// ZT_PEER_PATH_EXPIRATION（243 s）。所以「一上来就硬 reset」是必须被挡住的回归。

import (
	"testing"
	"time"
)

func withFastPathRecovery(t *testing.T, attempts int) {
	t.Helper()
	oldInterval, oldAttempts := pathReprobeInterval, pathReprobeAttempts
	pathReprobeInterval, pathReprobeAttempts = 20*time.Millisecond, attempts
	t.Cleanup(func() {
		pathReprobeInterval, pathReprobeAttempts = oldInterval, oldAttempts
	})
}

func waitForCond(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestJ_PathRecoverySoftThenHard：软探测跑满才升级硬 reset，且顺序是「先软后硬」。
func TestJ_PathRecoverySoftThenHard(t *testing.T) {
	const attempts = 3
	withFastPathRecovery(t, attempts)

	e, err := StartEngine(testConfig(pickFreePort(t)), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	if err := e.RebuildSocket("unit test"); err != nil {
		t.Fatalf("RebuildSocket failed: %v", err)
	}
	// 软探测必须先发生，硬 reset 不能在爆发之前出现（这是本次改动的全部意义）。
	waitForCond(t, "first soft reprobe", 2*time.Second, func() bool { return e.pathReprobes.Load() >= 1 })
	if got := e.pathHardResets.Load(); got != 0 {
		t.Fatalf("hard reset happened before the soft burst finished (count=%d)", got)
	}

	waitForCond(t, "hard reset after burst", 3*time.Second, func() bool { return e.pathHardResets.Load() > 0 })
	if got := e.pathReprobes.Load(); got != attempts {
		t.Fatalf("soft reprobes = %d, want %d", got, attempts)
	}
	if got := e.pathHardResets.Load(); got != 1 {
		t.Fatalf("hard resets = %d, want exactly 1", got)
	}
}

// TestK_StaleBurstDoesNotHardReset：第二次 rebind 让旧轮次失效（socketGen 变了），
// 旧轮次不能继续跑完并硬 reset —— 否则一次换网会触发多轮硬 reset，把端点反复清空。
func TestK_StaleBurstDoesNotHardReset(t *testing.T) {
	const attempts = 8 // 足够长，留给第二次 rebind 打断
	withFastPathRecovery(t, attempts)

	e, err := StartEngine(testConfig(pickFreePort(t)), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	if err := e.RebuildSocket("unit test #1"); err != nil {
		t.Fatalf("first RebuildSocket failed: %v", err)
	}
	waitForCond(t, "first burst started", 2*time.Second, func() bool { return e.pathReprobes.Load() >= 1 })

	if err := e.RebuildSocket("unit test #2"); err != nil {
		t.Fatalf("second RebuildSocket failed: %v", err)
	}
	gen := e.SocketGen()
	waitForCond(t, "second burst finished with hard reset", 3*time.Second, func() bool { return e.pathHardResets.Load() > 0 })
	if got := e.pathHardResets.Load(); got != 1 {
		t.Fatalf("hard resets = %d, want exactly 1 (stale burst must not escalate)", got)
	}
	if e.SocketGen() != gen {
		t.Fatalf("socketGen changed unexpectedly")
	}
}
