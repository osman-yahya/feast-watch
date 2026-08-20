package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxSourceSize caps the archive. This repository's source is a fraction of it;
// anything approaching it is a wrong URL or an error page, not a tree.
const maxSourceSize = 256 << 20

// maxSourceFile caps one file inside the archive, which is what stops a small
// download from becoming an unbounded write.
const maxSourceFile = 64 << 20

// FetchSource downloads the source tree for one ref and extracts it into dir.
//
// This is the last thing GitHub is still asked for, and it is asked for source
// rather than binaries: the mother compiles what it serves (see Build), so
// nothing has to be built anywhere else and no artifact has to be published for
// it to fetch. A tarball over HTTPS rather than `git clone` because it needs no
// git on the host — the mother already needs Go, and adding a second tool to
// the requirement list buys nothing here.
//
// The archive is not trusted. Every entry names its own destination, so a
// crafted one can name a path outside dir entirely; each is resolved and
// refused rather than escaped.
func FetchSource(ctx context.Context, client *http.Client, repoURL, ref, dir string) error {
	url := strings.TrimSuffix(repoURL, "/") + "/archive/refs/tags/" + ref + ".tar.gz"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("no source archive for %s: GET %s: %s", ref, url, resp.Status)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxSourceSize))
	if err != nil {
		return fmt.Errorf("source archive for %s is not gzip: %w", ref, err)
	}
	defer gz.Close()

	return extract(tar.NewReader(gz), dir)
}

// extract writes the archive into dir, dropping the single top-level directory
// the forge wraps a source tree in — it is named for the ref, so keeping it
// would make every path inside depend on which version is being built.
func extract(tr *tar.Reader, dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Only ordinary files and directories. A symlink in a source archive
		// has no business being followed by an extractor running as a service.
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			continue
		}

		// Refused before anything is derived from it. An absolute name, or one
		// with a ".." in it, is a malformed archive — and filepath.Join would
		// quietly absorb the leading slash and re-root it under dir, which
		// stores the surprise instead of reporting it.
		if err := refuseHostilePath(header.Name); err != nil {
			return err
		}

		rel := strip(header.Name)
		if rel == "" {
			continue
		}
		// Checked again on what is left, not only on what arrived. Dropping the
		// wrapper directory can expose a leading slash that was hidden behind
		// it — "wrapper//etc/passwd" is not an absolute name until the wrapper
		// is gone — and it is the remainder that becomes the path.
		if err := refuseHostilePath(rel); err != nil {
			return err
		}
		dest := filepath.Join(root, rel)
		// filepath.Join cleans the path, so this comparison is what actually
		// decides: a destination that is not under root is refused outright
		// rather than sanitised into something plausible.
		if dest != root && !strings.HasPrefix(dest, root+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the extraction directory", header.Name)
		}

		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := writeFile(tr, dest, header.FileInfo().Mode().Perm()); err != nil {
			return err
		}
	}
}

// refuseHostilePath rejects an entry name that could name anything but a path
// beneath the tree being extracted.
func refuseHostilePath(name string) error {
	clean := strings.TrimPrefix(name, "./")
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("archive entry %q is an absolute path", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("archive entry %q escapes the extraction directory", name)
		}
	}
	return nil
}

// strip removes the archive's single top-level directory from an entry name.
func strip(name string) string {
	name = strings.TrimPrefix(name, "./")
	_, rest, found := strings.Cut(name, "/")
	if !found {
		// The top-level directory entry itself, or a stray file beside it.
		return ""
	}
	return rest
}

func writeFile(r io.Reader, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, io.LimitReader(r, maxSourceFile+1))
	if err == nil && written > maxSourceFile {
		err = fmt.Errorf("archive entry %s exceeds %d bytes", dest, maxSourceFile)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}
