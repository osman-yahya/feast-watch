package agent

import (
	"encoding/pem"
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

func TestLoadConfigParsesTLSTrustKeys(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=https://10.0.0.1:8443\nTOKEN=t\nSERVER_NAME=s\nCA_FILE=/etc/feast-watch/ca.pem\nTLS_SKIP_VERIFY=true\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CAFile != "/etc/feast-watch/ca.pem" || !cfg.TLSSkipVerify {
		t.Fatalf("tls keys not parsed: %+v", cfg)
	}
}

func TestHTTPClientRejectsBadCAFile(t *testing.T) {
	if _, err := (Config{CAFile: "/nonexistent/ca.pem"}).HTTPClient(time.Second); err == nil {
		t.Fatal("unreadable CA_FILE must error")
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(p, []byte("not a pem"), 0o600)
	if _, err := (Config{CAFile: p}).HTTPClient(time.Second); err == nil {
		t.Fatal("invalid PEM must error")
	}
}

func TestHTTPClientTrustsCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	p := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(p, certPEM, 0o600)

	// default trust must reject the self-signed server
	plain, err := (Config{}).HTTPClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Get(srv.URL); err == nil {
		t.Fatal("sanity: default client should not trust the test server")
	}

	trusted, err := (Config{CAFile: p}).HTTPClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trusted.Get(srv.URL)
	if err != nil {
		t.Fatalf("CA_FILE-trusted client failed: %v", err)
	}
	resp.Body.Close()
}

func TestHTTPClientSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := (Config{TLSSkipVerify: true}).HTTPClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("skip-verify client failed: %v", err)
	}
	resp.Body.Close()
}
