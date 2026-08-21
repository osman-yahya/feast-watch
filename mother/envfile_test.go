package mother

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mother.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case this exists for: `feast-watch build` run by hand saw none of the
// configuration the service runs on, so a mother with a checkout named in its
// env file fetched source over the network instead.
func TestLoadEnvFileFillsInWhatWasNotGiven(t *testing.T) {
	t.Setenv("FW_SOURCE_DIR", "")
	path := writeEnv(t, "# the mother's config\nFW_SOURCE_DIR=/opt/feast-watch/src\n\nFW_API_KEY=\"quoted\"\n")

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FW_SOURCE_DIR"); got != "/opt/feast-watch/src" {
		t.Fatalf("FW_SOURCE_DIR: %q", got)
	}
	if got := os.Getenv("FW_API_KEY"); got != "quoted" {
		t.Fatalf("quotes are systemd's, and must not survive into the value: %q", got)
	}
}

// Explicit wins, or the file would fight the command line — and under systemd
// every key is already set, which is what makes this a no-op there.
func TestLoadEnvFileNeverOverridesTheEnvironment(t *testing.T) {
	t.Setenv("FW_DB_PATH", "/tmp/chosen.db")
	path := writeEnv(t, "FW_DB_PATH=/var/lib/feast-watch/mother.db\n")

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("FW_DB_PATH"); got != "/tmp/chosen.db" {
		t.Fatalf("the file overrode an explicit value: %q", got)
	}
}

// A developer machine has no deployment. Refusing to run there would be a
// worse answer than reading nothing.
func TestLoadEnvFileIsSilentWhenThereIsNone(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("a missing env file must not be an error: %v", err)
	}
}

// A malformed line is refused rather than skipped: systemd fails the unit on
// one, and a CLI that silently ignored what the service refuses to start with
// would send an operator looking in the wrong place.
func TestLoadEnvFileRefusesAMalformedLine(t *testing.T) {
	path := writeEnv(t, "FW_API_KEY=fine\nthis is not an assignment\n")

	err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("a malformed line must be reported")
	}
	if got := err.Error(); got == "" {
		t.Fatal("the error says nothing")
	}
}
