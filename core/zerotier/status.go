package zerotier

// RuntimeStatus is a point-in-time view of the ZeroTier runtime.
// Runtime liveness is supplied by the platform-specific implementation;
// Android/cgo reads the live Engine, while non-cgo builds report STOPPED.
type RuntimeStatus struct {
	State       string `json:"state"`
	NodeAddress string `json:"nodeAddress"`
	IPv4        string `json:"ipv4,omitempty"`
	Routes      int    `json:"routes"`
	NetworkID   string `json:"networkId,omitempty"`

	// Data-path diagnostics. These exist so a slow ZeroTier link can be
	// attributed to a specific stage instead of guessed at:
	//   framesDroppedC  — C ring queue full (oldest frame dropped)
	//   framesDroppedGo — Go frames channel full (newest frame dropped)
	//   wireSend*       — physical sendto() outcomes by errno class
	//   udpRcvBuf/SndBuf— buffers the kernel actually granted
	FramesDroppedC  uint64 `json:"framesDroppedC,omitempty"`
	FramesDroppedGo uint64 `json:"framesDroppedGo,omitempty"`
	WireSendOK      uint64 `json:"wireSendOk,omitempty"`
	WireSendEAGAIN  uint64 `json:"wireSendEagain,omitempty"`
	WireSendENOBUFS uint64 `json:"wireSendEnobufs,omitempty"`
	WireSendOther   uint64 `json:"wireSendOther,omitempty"`
	UDPRcvBuf       int    `json:"udpRcvBuf,omitempty"`
	UDPSndBuf       int    `json:"udpSndBuf,omitempty"`
}
