package mother

import (
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
)

func TestGenerateCLI(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	out, err := RunGenerate(st, "10.0.0.1:8443", []string{"--name=DB_Sunucusu"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "curl -sSLk https://10.0.0.1:8443/install/tk_") {
		t.Fatalf("generate output: %q", out)
	}
	// idempotent: same name returns the existing server's command
	out2, err := RunGenerate(st, "10.0.0.1:8443", []string{"--name=DB_Sunucusu"})
	if err != nil || out2 != out {
		t.Fatalf("generate must be idempotent: %v %q vs %q", err, out, out2)
	}
}
