// Package protocol defines the wire contract between agent and mother.
package protocol

type IngestRequest struct {
	Server       string             `json:"server"`
	AgentVersion string             `json:"agent_version"`
	Hostname     string             `json:"hostname,omitempty"`
	IP           string             `json:"ip,omitempty"`
	OS           string             `json:"os,omitempty"`
	Samples      map[string]float64 `json:"samples"`
}

type IngestResponse struct {
	Collectors     []string `json:"collectors"`
	Interval       int      `json:"interval"`
	DesiredVersion string   `json:"desired_version"`
}
