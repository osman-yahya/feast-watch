package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConf(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.conf")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigParsesKeyValues(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=https://10.0.0.1:8443\nTOKEN=tk_abc\nSERVER_NAME=DB_Sunucusu\n# comment\nCENTRIFUGO_CONNS_MAX=10000\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MotherURL != "https://10.0.0.1:8443" || cfg.Token != "tk_abc" || cfg.ServerName != "DB_Sunucusu" {
		t.Fatalf("got %+v", cfg)
	}
	if cfg.CentrifugoConnsMax != 10000 {
		t.Fatalf("conns max: %v", cfg.CentrifugoConnsMax)
	}
}

func TestLoadConfigFailsFastOnMissingRequired(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=https://10.0.0.1:8443\n")
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("expected error for missing TOKEN and SERVER_NAME")
	}
}

// The agent no longer carries CA_FILE or TLS_SKIP_VERIFY. Leftover keys in an
// agent.conf written by an older installer must be ignored rather than refused
// — the host is mid-migration and a hard failure there costs the very push
// that would report it.
func TestLoadConfigIgnoresRetiredTLSKeys(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=http://10.0.0.1:8443\nTOKEN=t\nSERVER_NAME=s\nCA_FILE=/etc/feast-watch/ca.pem\nTLS_SKIP_VERIFY=true\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("retired keys must not break config load: %v", err)
	}
	if cfg.MotherURL != "http://10.0.0.1:8443" || cfg.Token != "t" {
		t.Fatalf("got %+v", cfg)
	}
}

// Every request concatenates a rooted path onto MOTHER_URL, so a value without
// a scheme fails on every push with nothing in the config to point at. Catch it
// at startup instead.
func TestLoadConfigRejectsUnusableMotherURL(t *testing.T) {
	for _, raw := range []string{"10.0.0.1:8443", "ftp://10.0.0.1:8443", "http://"} {
		p := writeConf(t, "MOTHER_URL="+raw+"\nTOKEN=t\nSERVER_NAME=s\n")
		if _, err := LoadConfig(p); err == nil {
			t.Fatalf("MOTHER_URL %q must be rejected", raw)
		}
	}
}

// A mother behind a TLS-terminating proxy is still addressed over https; the
// agent just uses the system trust store to get there.
func TestHTTPClientUsesSystemTrustOnly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := Config{}.HTTPClient(time.Second)
	if client.Timeout != time.Second {
		t.Fatalf("timeout not applied: %v", client.Timeout)
	}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("an untrusted certificate must still be refused")
	}
}
