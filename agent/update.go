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

const (
	maxBinarySize   = 256 << 20 // 256 MB cap on downloaded agent binaries
	maxChecksumSize = 1 << 10   // 1 KB cap on .sha256 files
)

const (
	binaryPath   = "/download/agent/"
	checksumPath = "/download/agent/"
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

	fetch := func(path string, limit int64) ([]byte, error) {
		resp, err := client.Get(cfg.MotherURL + path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %d", path, resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("download exceeds %d bytes", limit)
		}
		return data, nil
	}

	binary, err := fetch(binaryPath+desiredVersion, maxBinarySize)
	if err != nil {
		return err
	}
	sumRaw, err := fetch(checksumPath+desiredVersion+".sha256", maxChecksumSize)
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
