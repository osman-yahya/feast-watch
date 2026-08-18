package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
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
	return SelfUpdateWithClient(cfg, desiredVersion, exit, &http.Client{Timeout: 60 * time.Second})
}

// SelfUpdateWithClient is SelfUpdate with a caller-supplied client, so the
// download runs on the same transport policy as the push loop with its own
// timeout — a binary transfer must not give up as fast as a push does.
func SelfUpdateWithClient(cfg Config, desiredVersion string, exit func(int), client *http.Client) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return selfUpdate(cfg, desiredVersion, self, exit, client)
}

func selfUpdate(cfg Config, desiredVersion, target string, exit func(int), client *http.Client) error {
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

	// The download is keyed by version *and* platform. GOARCH alone is not
	// enough: the mother stages builds for several operating systems, and a
	// Windows agent asking for "v1.3.0-amd64" would overwrite itself with the
	// Linux binary of the same architecture.
	versionPlatform := desiredVersion + "-" + runtime.GOOS + "-" + runtime.GOARCH

	binary, err := fetch(binaryPath+versionPlatform, maxBinarySize)
	if err != nil {
		return err
	}
	sumRaw, err := fetch(checksumPath+versionPlatform+".sha256", maxChecksumSize)
	if err != nil {
		return err
	}
	wantSum := strings.TrimSpace(string(sumRaw))

	h := sha256.Sum256(binary)
	if hex.EncodeToString(h[:]) != wantSum {
		return fmt.Errorf("checksum mismatch for %s: refusing update", versionPlatform)
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
