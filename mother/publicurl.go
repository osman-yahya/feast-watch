package mother

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultPublicURL is where a mother with nothing configured tells agents to
// reach it. Plain HTTP: the mother does not terminate TLS.
const DefaultPublicURL = "http://127.0.0.1:8443"

// PublicURL resolves the base URL agents are handed, from FW_PUBLIC_URL with
// the retired FW_PUBLIC_ADDR as a fallback.
//
// It is one URL rather than a scheme plus a host:port because the mother no
// longer decides the scheme — it serves plain HTTP and may sit behind
// something that terminates TLS at a different hostname, port or path prefix.
// A full URL is the only shape that can describe that; the agent concatenates
// its paths straight onto this value.
//
// legacyAddr (FW_PUBLIC_ADDR) is honoured as plain HTTP for one release rather
// than ignored: silently falling through to the default would hand every agent
// 127.0.0.1 and break the fleet with no error to read. Callers should warn when
// they supply it.
func PublicURL(raw, legacyAddr string) (string, error) {
	raw = strings.TrimSpace(raw)
	legacyAddr = strings.TrimSpace(legacyAddr)

	switch {
	case raw == "" && legacyAddr == "":
		return DefaultPublicURL, nil
	case raw == "":
		raw = "http://" + legacyAddr
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("FW_PUBLIC_URL %q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf(
			"FW_PUBLIC_URL %q must start with http:// or https:// (a bare host:port no longer works — the mother does not choose the scheme)", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("FW_PUBLIC_URL %q has no host", raw)
	}
	// Every consumer appends a rooted path ("/install/…", "/v1/ingest"), so a
	// trailing slash here would produce a doubled separator.
	return strings.TrimSuffix(parsed.String(), "/"), nil
}
