package collectors

import (
	"context"
	"testing"
	"time"
)

func TestCPUCollect(t *testing.T) {
	c := NewCPU()
	c.percent = func(interval time.Duration, percpu bool) ([]float64, error) {
		return []float64{34.2}, nil
	}
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "cpu.usage" || got[0].Value != 34.2 {
		t.Fatalf("got %+v", got)
	}
}
