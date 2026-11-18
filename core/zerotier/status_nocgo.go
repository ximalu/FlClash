//go:build !cgo

package zerotier

import "time"

// GetRuntimeStatus keeps the package/API portable for non-cgo builds where
// the Android ZeroTier Engine is not present.
//
// 没有引擎就没有任何链路证据：LinkHealth=unknown、Peers=nil（不是空数组）、
// socket/自愈字段全部缺省——调用方必须当未知处理。
func GetRuntimeStatus() RuntimeStatus {
	status := RuntimeStatus{
		State:      "STOPPED",
		UpdatedAt:  time.Now().UnixMilli(),
		LinkHealth: healthUnknown,
		Peers:      nil,
	}
	if msg, at := currentStartError(); msg != "" {
		status.LastError = msg
		status.LastErrorAt = at
	}
	return status
}
