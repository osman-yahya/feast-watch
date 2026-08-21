package mother

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultEnvFile is where deploy/mother-install.sh writes the mother's
// configuration, and what its unit names in EnvironmentFile=.
const DefaultEnvFile = "/etc/feast-watch/mother.env"

// LoadEnvFile fills in variables this process was not given, from the file the
// deployment already keeps them in.
//
// Under systemd nothing here does anything: the unit's EnvironmentFile= has
// already put every key in the environment, and set values are never replaced.
// It exists for the other way this binary is run — an operator typing
// `feast-watch build v1.1.0` — which until now saw none of that file. The
// result was a command that read FW_DB_PATH from its own command line and
// FW_SOURCE_DIR from nowhere, then quietly fetched source over the network on a
// host that had a checkout sitting right there, configured, in the file the
// service reads.
//
// Explicit still wins. A variable already in the environment is left alone, so
// naming one on the command line overrides the file rather than fighting it.
//
// A missing file is not an error: the CLI runs on developer machines that have
// no deployment at all.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s:%d: %q is not KEY=VALUE", path, line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: no key before '='", path, line)
		}
		if os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, unquote(strings.TrimSpace(value))); err != nil {
			return err
		}
	}
	return sc.Err()
}

// unquote drops one layer of matching quotes, which systemd's EnvironmentFile
// parser also accepts — so a file that works for the unit works here.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
