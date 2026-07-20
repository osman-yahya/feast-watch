package collectors

import (
	"context"
	"fmt"
	"net"
	"testing"
)

// fakeRESP answers any command with a canned bulk-string INFO payload.
func fakeRESP(t *testing.T, info string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				c.Read(buf)
				fmt.Fprintf(c, "$%d\r\n%s\r\n", len(info), info)
			}(conn)
		}
	}()
	return ln
}

func TestDragonflyParsesInfo(t *testing.T) {
	info := "# Memory\r\nused_memory:1073741824\r\nmaxmemory:2147483648\r\n# Clients\r\nconnected_clients:42\r\n"
	ln := fakeRESP(t, info)
	defer ln.Close()

	d := NewDragonfly(ln.Addr().String())
	got, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["dragonfly.mem_used"] != 1073741824 || byKey["dragonfly.mem_max"] != 2147483648 || byKey["dragonfly.clients"] != 42 {
		t.Fatalf("got %v", byKey)
	}
}
