package agent

import (
	"os"
	"path/filepath"
	"testing"
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
