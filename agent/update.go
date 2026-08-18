package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/osman-yahya/feast-watch/shared/release"
)

const (
	maxBinarySize   = 256 << 20 // 256 MB cap on downloaded agent binaries
	maxChecksumSize = 1 << 10   // 1 KB cap on .sha256 files
)

// SelfUpdate downloads desiredVersion from the release host, verifies its
// SHA-256, atomically replaces the running executable and exits 0 — the
// service manager restarts us.
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

// selfUpdate fetches the build for this platform from the release host.
//
// The binary comes from GitHub Releases, never from the mother. The mother
// names a version and nothing more, which keeps binary distribution off the
// monitoring path entirely: the mother stores no builds, serves no bytes, and
// a rollout cannot be blocked by a file somebody forgot to stage on it.
func selfUpdate(cfg Config, desiredVersion, target string, exit func(int), client *http.Client) error {
	asset := release.AssetName(runtime.GOOS, runtime.GOARCH)
	base := cfg.releaseBaseURL()

	// The checksum is fetched first, and it is small. A tag that was never
	// published, or one with no build for this platform, fails here for the
	// price of one request instead of after a whole binary transfer.
	sumRaw, err := fetch(client, release.DownloadURL(base, desiredVersion, asset+release.ChecksumSuffix), maxChecksumSize)
	if err != nil {
		return err
	}
	wantSum, err := release.ParseChecksum(sumRaw)
	if err != nil {
		return fmt.Errorf("checksum for %s %s: %w", desiredVersion, asset, err)
	}

	tmp, gotSum, err := download(client, release.DownloadURL(base, desiredVersion, asset), target)
	if err != nil {
		return err
	}
	// From here every failure has to remove the temporary file: nothing else
	// ever sweeps it, so a stranded one accumulates on each retry.
	defer os.Remove(tmp)

	if gotSum != wantSum {
		return fmt.Errorf("checksum mismatch for %s %s: refusing update", desiredVersion, asset)
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	exit(0)
	return nil
}

// fetch reads a small resource fully into memory, refusing anything past limit.
func fetch(client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
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

// download streams the asset to a temporary file beside target, hashing as it
// goes, and returns the temp path and the digest.
//
// Streaming rather than reading into memory: the agent runs on the hosts it
// monitors, and holding a whole binary in RAM on a small VPS is an OOM that
// kills the process being updated. The temp file is created in target's
// directory so the rename that follows stays on one filesystem and is atomic.
func download(client *http.Client, url, target string) (string, string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.new")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, maxBinarySize+1))
	if err == nil && written > maxBinarySize {
		err = fmt.Errorf("download exceeds %d bytes", maxBinarySize)
	}
	if err == nil {
		// The replacement has to be executable, and durable: without the sync
		// a power loss after the rename can leave a truncated file as the
		// agent binary, which the service manager then respawns against.
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return tmp, hex.EncodeToString(hash.Sum(nil)), nil
}
