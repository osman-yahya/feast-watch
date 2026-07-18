package protocol

import (
	"encoding/json"
	"testing"
)

func TestIngestRequestRoundTrip(t *testing.T) {
	in := IngestRequest{
		Server: "centrifugo-1", AgentVersion: "1.2.0",
		Hostname: "cf1", IP: "10.0.0.5", OS: "linux",
		Samples: map[string]float64{"cpu.usage": 34.2, "centrifugo.conns": 4812},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out IngestRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Server != in.Server || out.Samples["cpu.usage"] != 34.2 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestIngestResponseJSONFields(t *testing.T) {
	b, _ := json.Marshal(IngestResponse{Collectors: []string{"cpu"}, Interval: 10, DesiredVersion: "1.3.0"})
	want := `{"collectors":["cpu"],"interval":10,"desired_version":"1.3.0"}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}
