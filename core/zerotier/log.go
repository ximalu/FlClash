// Package zerotier embeds a ZeroTier core (libzerotiercore.a) behind a thin
// C ABI wrapper and exposes a Go engine (node lifecycle, physical UDP wire,
// config/routes snapshot) plus a pure-Go L2/L3 adapter that connects the
// Android TUN to the ZeroTier virtual network.
//
// The adapter consumes managed routes ONLY from the ZT config callback
// (never hardcodes CIDRs), resolves peer MACs with ARP/NDP, and queues
// packets awaiting resolution.
package zerotier

// Logger is injectable so the package stays free of mihomo dependencies.
// The Android core sets it to mihomo's logger in lib.go; standalone tests
// leave it unset (silent).
var Logger func(level, format string, args ...any)

func logf(level, format string, args ...any) {
	if Logger != nil {
		Logger(level, format, args...)
	}
}

// Infof logs at info level (if a Logger is installed).
func Infof(format string, args ...any) { logf("info", format, args...) }

// Warnf logs at warn level (if a Logger is installed).
func Warnf(format string, args ...any) { logf("warn", format, args...) }
