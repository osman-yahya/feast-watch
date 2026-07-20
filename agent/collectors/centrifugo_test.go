package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCentrifugoSumsClientsAcrossNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "cfkey" {
			t.Errorf("missing api key header")
		}
		w.Write([]byte(`{"result":{"nodes":[{"num_clients":3000},{"num_clients":1812}]}}`))
	}))
	defer srv.Close()

	c := NewCentrifugo(srv.URL, "cfkey", 10000)
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["centrifugo.conns"] != 4812 || byKey["centrifugo.conns_max"] != 10000 {
		t.Fatalf("got %v", byKey)
	}
}
