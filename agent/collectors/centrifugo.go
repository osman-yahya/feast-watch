package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Centrifugo reads total client connections from the server `info` API and
// reports them against the configured maximum ("conns vs max" family).
type Centrifugo struct {
	apiURL   string
	apiKey   string
	connsMax float64
	client   *http.Client
}

func NewCentrifugo(apiURL, apiKey string, connsMax float64) *Centrifugo {
	return &Centrifugo{apiURL: apiURL, apiKey: apiKey, connsMax: connsMax,
		client: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Centrifugo) Name() string { return "centrifugo" }

func (c *Centrifugo) Collect(ctx context.Context) ([]Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL,
		bytes.NewReader([]byte(`{"method":"info","params":{}}`)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("centrifugo info: %d", resp.StatusCode)
	}

	var out struct {
		Result struct {
			Nodes []struct {
				NumClients float64 `json:"num_clients"`
			} `json:"nodes"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	total := 0.0
	for _, n := range out.Result.Nodes {
		total += n.NumClients
	}
	return []Sample{
		{Key: "centrifugo.conns", Value: total},
		{Key: "centrifugo.conns_max", Value: c.connsMax},
	}, nil
}
