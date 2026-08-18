package mother

import "testing"

func TestPublicURLDefaultsToLocalPlainHTTP(t *testing.T) {
	got, err := PublicURL("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8443" {
		t.Fatalf("default: %q", got)
	}
}

// FW_PUBLIC_ADDR was a bare host:port because the scheme was derived from the
// TLS cert. With TLS gone the scheme has to be stated, so the old variable is
// honoured for one release as plain HTTP rather than silently ignored — which
// would hand every agent the 127.0.0.1 default.
func TestPublicURLAcceptsLegacyAddrAsPlainHTTP(t *testing.T) {
	got, err := PublicURL("", "10.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://10.0.0.1:8443" {
		t.Fatalf("legacy addr: %q", got)
	}
}

func TestPublicURLPrefersExplicitURL(t *testing.T) {
	got, err := PublicURL("http://mother:8443", "10.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://mother:8443" {
		t.Fatalf("explicit URL must win: %q", got)
	}
}

// The mother no longer terminates TLS, but it can sit behind something that
// does. Keeping https valid here is what lets that deployment be described at
// all — the agent just concatenates paths onto whatever it is given.
func TestPublicURLAllowsHTTPSForAFrontingProxy(t *testing.T) {
	got, err := PublicURL("https://ops.feast.tr/watch/", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://ops.feast.tr/watch" {
		t.Fatalf("trailing slash must be stripped: %q", got)
	}
}

func TestPublicURLRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{
		"10.0.0.1:8443", // bare host:port — the scheme is no longer inferable
		"ftp://10.0.0.1:8443",
		"http://",
		"://nope",
	} {
		if got, err := PublicURL(raw, ""); err == nil {
			t.Fatalf("%q must be rejected, got %q", raw, got)
		}
	}
}
