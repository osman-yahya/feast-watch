// Package mirror lets the mother stand between the agents and GitHub Releases.
//
// Agents used to download their own binaries straight from the public release,
// and that was the better arrangement while they could reach it: binary
// distribution stayed off the monitoring path entirely, so the mother stored no
// builds, served no bytes, and a rollout could not be blocked by a file nobody
// staged. This exists because that premise stopped holding — on this fleet the
// agents have no route to the internet at all, and a rollout they cannot fetch
// is not a rollout.
//
// So the mother fetches instead, on the first request for a build, verifies it
// against the checksum GitHub published, and keeps it. The chain the agent
// relies on is unbroken: CI computes the checksum, the mother refuses anything
// that does not match it, and the agent verifies again before replacing itself.
// What the mother adds is a hop, not a new authority — it never builds anything
// and never signs its own work.
//
// The cost is named rather than hidden: binary distribution is now on the
// monitoring path. The mother's disk and its uptime decide whether a rollout
// can land.
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// Cache holds verified release assets on disk, fetching each one the first time
// it is asked for.
type Cache struct {
	dir     string
	baseURL string
	client  *http.Client

	// mu guards inflight, which holds one lock per (tag, asset) being fetched.
	// A group rollout points every member at one version at once, so the first
	// request for a build arrives many times over before any of them finishes;
	// without this, thirty agents mean thirty transfers of the same file.
	mu       sync.Mutex
	inflight map[string]*sync.Mutex
}

func New(dir, baseURL string, client *http.Client) *Cache {
	return &Cache{dir: dir, baseURL: baseURL, client: client, inflight: map[string]*sync.Mutex{}}
}

// Ensure returns the local path of a verified asset, fetching it if this is the
// first time it has been asked for.
func (c *Cache) Ensure(tag, asset string) (string, error) {
	dest := filepath.Join(c.dir, tag, asset)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	unlock := c.lockFor(tag + "/" + asset)
	defer unlock()

	// Re-checked under the lock: whoever held it before us may have been
	// fetching exactly this, in which case there is nothing left to do.
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	// Place fetches the published checksum first and refuses anything that does
	// not match it, so nothing unverified is ever written here.
	if err := selfupdate.Place(c.client, c.baseURL, tag, asset, dest); err != nil {
		// Leave no half-made directory: an empty one would read as a hit on
		// the next call and serve nothing.
		os.Remove(filepath.Dir(dest))
		return "", err
	}
	if err := c.writeChecksum(dest); err != nil {
		os.Remove(dest)
		os.Remove(filepath.Dir(dest))
		return "", err
	}
	return dest, nil
}

// writeChecksum stores the digest of the file that was actually kept, rather
// than re-fetching the published one. They are the same value — Place refused
// the transfer otherwise — and this way what the mother serves describes what
// the mother holds.
func (c *Cache) writeChecksum(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	return os.WriteFile(path+release.ChecksumSuffix, []byte(sum+"\n"), 0o644)
}

// lockFor serialises callers asking for the same asset, and returns the
// release. The per-key lock is dropped once nobody is waiting on it, so a long
// -lived mother does not accumulate one mutex per build it has ever served.
func (c *Cache) lockFor(key string) func() {
	c.mu.Lock()
	lock, ok := c.inflight[key]
	if !ok {
		lock = &sync.Mutex{}
		c.inflight[key] = lock
	}
	c.mu.Unlock()

	lock.Lock()
	return func() {
		lock.Unlock()
		c.mu.Lock()
		// Only drop it when it is free; another caller may already be blocked.
		if lock.TryLock() {
			lock.Unlock()
			delete(c.inflight, key)
		}
		c.mu.Unlock()
	}
}

// Verify is what a caller uses to state the obvious precondition: a name that
// is not a build this project publishes must never reach the filesystem.
func Verify(asset string) error {
	if _, _, ok := release.AssetKindOf(asset); !ok {
		return fmt.Errorf("%q is not a published build", asset)
	}
	return nil
}
