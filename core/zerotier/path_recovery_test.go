//go:build linux && !android && cgo

package zerotier

// 换网路径自愈的回归防线（0.9.22）：rebind 之后先「经 root 转发一次 HELLO」
// 让对端学到我们的新端点，再持续软探测已记住的端点；**不再**自动硬 reset
// （它会把 lr 清零，下一轮删掉 path、端点地址一起忘掉）。
//
// 背景：issues-backlog #10 / #15。2026-11-26 实测，硬 reset 后 i3 侧
// `paths=0`，蜂窝方向要等一个 60 s HELLO 周期（71 s 恢复）。

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

// TestJ_PathRecoveryHelloThenProbe：rebind 后先有 relayed HELLO，再跑满软探测；
// 整个过程不得发生自动硬 reset。
func TestJ_PathRecoveryHelloThenProbe(t *testing.T) {
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
	waitForCond(t, "relayed HELLO", 2*time.Second, func() bool { return e.pathRehellos.Load() >= 1 })
	waitForCond(t, "soft burst finished", 3*time.Second, func() bool {
		return e.pathReprobes.Load() >= attempts
	})
	if got := e.pathHardResets.Load(); got != 0 {
		t.Fatalf("automatic hard reset happened (count=%d); it must only be explicit (ForcePathReset)", got)
	}
}

// TestK_StaleBurstDoesNotEscalate：第二次 rebind 让旧轮次失效（socketGen 变了），
// 旧轮次不能再探测，也不应造成任何硬 reset。
func TestK_StaleBurstDoesNotEscalate(t *testing.T) {
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
	waitForCond(t, "second burst finished", 3*time.Second, func() bool { return e.pathReprobes.Load() >= attempts+1 })
	if got := e.pathRehellos.Load(); got != 2 {
		t.Fatalf("relayed HELLOs = %d, want exactly 2 (one per rebind)", got)
	}
	if got := e.pathHardResets.Load(); got != 0 {
		t.Fatalf("automatic hard resets = %d, want 0", got)
	}
	if e.SocketGen() != gen {
		t.Fatalf("socketGen changed unexpectedly")
	}
}
