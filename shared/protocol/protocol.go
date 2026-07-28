// Package protocol defines the wire contract between agent and mother.
package protocol

type IngestRequest struct {
	Server       string `json:"server"`
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname,omitempty"`
	IP           string `json:"ip,omitempty"`
	OS           string `json:"os,omitempty"`
	// Capabilities lists the collectors this agent actually registered.
	// Service collectors (postgres, dragonfly, centrifugo, k8s) register only
	// when agent.conf configures them, so the mother has no other way to know
	// that enabling one on this server would silently collect nothing.
	// Sent with the identity fields on the first push only.
	Capabilities []string           `json:"capabilities,omitempty"`
	Samples      map[string]float64 `json:"samples"`
}

type IngestResponse struct {
	Collectors     []string `json:"collectors"`
	Interval       int      `json:"interval"`
	DesiredVersion string   `json:"desired_version"`
}
