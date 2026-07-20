package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// SelfUpdate downloads desiredVersion from the mother, verifies its SHA-256,
// atomically replaces the running executable and exits 0 — systemd restarts us.
func SelfUpdate(cfg Config, desiredVersion string, exit func(int)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return selfUpdate(cfg, desiredVersion, self, exit)
}

func selfUpdate(cfg Config, desiredVersion, target string, exit func(int)) error {
	client := &http.Client{Timeout: 60 * time.Second}

	fetch := func(path string) ([]byte, error) {
		resp, err := client.Get(cfg.MotherURL + path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %d", path, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	binary, err := fetch("/download/agent/" + desiredVersion)
	if err != nil {
		return err
	}
	sumRaw, err := fetch("/download/agent/" + desiredVersion + ".sha256")
	if err != nil {
		return err
	}
	wantSum := strings.TrimSpace(string(sumRaw))

	h := sha256.Sum256(binary)
	if hex.EncodeToString(h[:]) != wantSum {
		return fmt.Errorf("checksum mismatch for %s: refusing update", desiredVersion)
	}

	tmp := target + ".new"
	if err := os.WriteFile(tmp, binary, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	exit(0)
	return nil
}
