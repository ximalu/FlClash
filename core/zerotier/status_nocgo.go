//go:build !cgo

package zerotier

// GetRuntimeStatus keeps the package/API portable for non-cgo builds where
// the Android ZeroTier Engine is not present.
func GetRuntimeStatus() RuntimeStatus {
	return RuntimeStatus{State: "STOPPED"}
}
