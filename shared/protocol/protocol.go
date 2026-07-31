// Package protocol defines the wire contract between agent and mother.
package protocol

type IngestRequest struct {
	Server       string `json:"server"`
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname,omitempty"`
	IP           string `json:"ip,omitempty"`
	OS           string `json:"os,omitempty"`
	// Arch is the agent's GOARCH. Together with OS it identifies which agent
	// binary this host can actually run, which is what lets the mother reject
	// a desired version that has no build for this platform instead of
	// handing the agent a download it cannot execute.
	// Sent with the identity fields on the first push only.
	Arch string `json:"arch,omitempty"`
	// Capabilities lists the collectors this agent actually registered.
	// Service collectors (postgres, dragonfly, centrifugo, k8s) register only
	// when agent.conf configures them, so the mother has no other way to know
	// that enabling one on this server would silently collect nothing.
	// Sent with the identity fields on the first push only.
	Capabilities []string `json:"capabilities,omitempty"`
	// UpdateError carries the last self-update failure, empty when the last
	// attempt succeeded or none was made. Without it a failed update is
	// invisible: the panel would show a target version the agent silently
	// never reaches.
	UpdateError string             `json:"update_error,omitempty"`
	Samples     map[string]float64 `json:"samples"`
}

type IngestResponse struct {
	Collectors     []string `json:"collectors"`
	Interval       int      `json:"interval"`
	DesiredVersion string   `json:"desired_version"`
}
