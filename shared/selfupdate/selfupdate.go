// Package selfupdate downloads a released binary, verifies its published
// SHA-256, and puts it somewhere.
//
// It does not restart anything, and it does not care what the binary is. The
// agent points it at its own executable and then exits; the mother points it at
// a staging path inside its state directory, because a sandboxed service cannot
// write its own binary and a root helper installs it at the next start.
//
// Sharing the transfer is what makes "the mother updates the way the agent
// does" a fact about the code rather than a claim in a comment: the size caps,
// the fsync before the rename and the 0755 are exactly the details a second
// copy would lose.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/osman-yahya/feast-watch/shared/release"
)

// MaxBinarySize caps a downloaded binary. Exported because it is a limit
// callers test against: the agent asserts its own update path refuses an
// oversized asset, and that assertion has to name the same number the transfer
// enforces rather than a copy of it.
const MaxBinarySize = 256 << 20 // 256 MB

// maxChecksumSize caps a .sha256 file. Nothing outside this package has a
// reason to know it.
const maxChecksumSize = 1 << 10 // 1 KB

// Place fetches asset from the tagged release, verifies it against the
// published checksum, and renames it over dest.
//
// The checksum comes first, and it is small. A tag that was never published,
// or one with no build for this platform, fails here for the price of one
// request instead of after a whole binary transfer.
func Place(client *http.Client, baseURL, tag, asset, dest string) error {
	sumRaw, err := fetch(client, release.DownloadURL(baseURL, tag, asset+release.ChecksumSuffix), maxChecksumSize)
	if err != nil {
		return err
	}
	wantSum, err := release.ParseChecksum(sumRaw)
	if err != nil {
		return fmt.Errorf("checksum for %s %s: %w", tag, asset, err)
	}

	tmp, gotSum, err := download(client, release.DownloadURL(baseURL, tag, asset), dest)
	if err != nil {
		return err
	}
	// From here every failure has to remove the temporary file: nothing else
	// ever sweeps it, so a stranded one accumulates on each retry.
	defer os.Remove(tmp)

	if gotSum != wantSum {
		return fmt.Errorf("checksum mismatch for %s %s: refusing update", tag, asset)
	}
	return os.Rename(tmp, dest)
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
func download(client *http.Client, url, dest string) (string, string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.new")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, MaxBinarySize+1))
	if err == nil && written > MaxBinarySize {
		err = fmt.Errorf("download exceeds %d bytes", MaxBinarySize)
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
