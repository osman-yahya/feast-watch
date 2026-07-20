package collectors

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Dragonfly issues a raw RESP INFO over TCP — no redis client dependency.
// "RAM runs out → it stops", so memory vs maxmemory is the headline metric.
type Dragonfly struct {
	addr string
}

func NewDragonfly(addr string) *Dragonfly { return &Dragonfly{addr: addr} }

func (d *Dragonfly) Name() string { return "dragonfly" }

func (d *Dragonfly) Collect(ctx context.Context) ([]Sample, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte("INFO\r\n")); err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn)
	header, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(header, "$") {
		return nil, fmt.Errorf("unexpected INFO reply: %q", header)
	}
	size, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil || size <= 0 {
		return nil, fmt.Errorf("bad INFO length: %q", header)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	fields := map[string]float64{}
	for _, line := range strings.Split(string(body), "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			fields[k] = f
		}
	}
	return []Sample{
		{Key: "dragonfly.mem_used", Value: fields["used_memory"]},
		{Key: "dragonfly.mem_max", Value: fields["maxmemory"]},
		{Key: "dragonfly.clients", Value: fields["connected_clients"]},
	}, nil
}
