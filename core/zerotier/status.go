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
}
