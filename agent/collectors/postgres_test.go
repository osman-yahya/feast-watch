package collectors

import (
	"context"
	"testing"
)

func TestPostgresReportsConnsVsMax(t *testing.T) {
	p := NewPostgres("postgres://ignored")
	p.query = func(ctx context.Context) (conns, connsMax float64, err error) {
		return 42, 100, nil
	}
	got, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["postgres.conns"] != 42 || byKey["postgres.conns_max"] != 100 {
		t.Fatalf("got %v", byKey)
	}
}
