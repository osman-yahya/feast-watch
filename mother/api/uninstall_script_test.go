package api

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

// This is the one test that closes the loop the two-phase delete is built on:
// the real uninstall script, run against a real mother, removing a real (temp)
// install tree and reporting it — which is what actually makes the panel row
// disappear. Everything else about the flow is covered in pieces; nothing else
// covers the pieces fitting together.
//
// It runs the script through FW_ROOT, so it needs neither root nor systemd
// (the same device e2e/colocation_test.sh uses).
func TestUninstallScriptReportsTheRemovalAndDropsTheRow(t *testing.T) {
	requireScriptTools(t)

	a, st := setup(t)
	mother := httptest.NewServer(a.Handler())
	defer mother.Close()

	srv, err := st.AddServer("web-1")
	if err != nil {
		t.Fatal(err)
	}
	// A server that has pushed is one with an agent on it, which is what makes
	// the delete two-phase rather than immediate.
	postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", Samples: map[string]float64{"cpu.usage": 1},
	})
	if w := deleteServer(t, a, srv.ID, ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}

	root := fakeInstallTree(t, mother.URL, srv.Token)
	out, err := runUninstaller(t, root)
	if err != nil {
		t.Fatalf("uninstaller failed: %v\n%s", err, out)
	}

	if _, err := st.ServerByID(srv.ID); err != store.ErrNotFound {
		t.Fatalf("the confirmed removal did not drop the row (err=%v)\nscript output:\n%s", err, out)
	}
	for _, path := range []string{
		"usr/local/bin/feast-watch-agent",
		"etc/feast-watch/agent.conf",
		"usr/local/sbin/feast-watch-agent-uninstall",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the uninstall", path)
		}
	}
}

// An uninstaller that cannot reach the mother must still finish removing the
// host. The row is left behind for "Zorla Sil" — the alternative, aborting,
// would leave an agent running on a machine somebody has already decommissioned.
func TestUninstallScriptFinishesWhenTheMotherIsUnreachable(t *testing.T) {
	requireScriptTools(t)

	// A port nothing is listening on: the httptest server is created and
	// closed, so its address is a definitively dead one.
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()

	root := fakeInstallTree(t, deadURL, "tk_whatever")
	out, err := runUninstaller(t, root)
	if err != nil {
		t.Fatalf("uninstaller must not fail on an unreachable mother: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/local/bin/feast-watch-agent")); !os.IsNotExist(err) {
		t.Fatal("the binary survived an uninstall the mother did not hear about")
	}
}

// requireScriptTools skips where the script cannot be exercised honestly.
func requireScriptTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	// With systemd present the script would act on the real machine's units
	// rather than on the temp tree — FW_ROOT cannot redirect systemctl.
	if _, err := exec.LookPath("systemctl"); err == nil {
		t.Skip("systemctl present: the script would touch this machine's units")
	}
}

// fakeInstallTree lays out what the installer would have created, under a temp
// root the script can be pointed at with FW_ROOT.
func fakeInstallTree(t *testing.T, motherURL, token string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"usr/local/bin", "usr/local/sbin", "etc/feast-watch", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("usr/local/bin/feast-watch-agent", "#!/bin/sh\n", 0o755)
	write("etc/systemd/system/feast-watch-agent.service", "[Unit]\n", 0o644)
	write("etc/feast-watch/agent.conf",
		fmt.Sprintf("MOTHER_URL=%s\nTOKEN=%s\nSERVER_NAME=web-1\n", motherURL, token), 0o600)
	write("usr/local/sbin/feast-watch-agent-uninstall", uninstallScript, 0o755)
	return root
}

func runUninstaller(t *testing.T, root string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "usr/local/sbin/feast-watch-agent-uninstall"),
		"--purge", "--report")
	cmd.Env = append(os.Environ(), "FW_ROOT="+root)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
