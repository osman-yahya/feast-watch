// Package agent implements the feast-watch agent: config, push loop, self-update.
package agent

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MotherURL  string
	Token      string
	ServerName string

	CentrifugoAPIURL   string
	CentrifugoAPIKey   string
	CentrifugoConnsMax float64
	DragonflyAddr      string
	PostgresDSN        string
	K8sAPIURL          string
	K8sToken           string
}

// LoadConfig reads KEY=VALUE lines ('#' comments allowed) and validates
// required keys at startup — fail fast, never run half-configured.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("malformed line %q", line)
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}

	cfg := Config{
		MotherURL:        kv["MOTHER_URL"],
		Token:            kv["TOKEN"],
		ServerName:       kv["SERVER_NAME"],
		CentrifugoAPIURL: kv["CENTRIFUGO_API_URL"],
		CentrifugoAPIKey: kv["CENTRIFUGO_API_KEY"],
		DragonflyAddr:    kv["DRAGONFLY_ADDR"],
		PostgresDSN:      kv["POSTGRES_DSN"],
		K8sAPIURL:        kv["K8S_API_URL"],
		K8sToken:         kv["K8S_TOKEN"],
	}
	if raw := kv["CENTRIFUGO_CONNS_MAX"]; raw != "" {
		cfg.CentrifugoConnsMax, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("CENTRIFUGO_CONNS_MAX: %w", err)
		}
	}

	var missing []string
	for k, v := range map[string]string{"MOTHER_URL": cfg.MotherURL, "TOKEN": cfg.Token, "SERVER_NAME": cfg.ServerName} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	if err := validateMotherURL(cfg.MotherURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateMotherURL fails fast on a MOTHER_URL the agent could never reach.
// Every request is built by concatenating a rooted path onto this value, so a
// bare host:port or a missing scheme produces a request error on every push
// with nothing in the config to point at.
func validateMotherURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("MOTHER_URL %q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("MOTHER_URL %q must start with http:// or https://", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("MOTHER_URL %q has no host", raw)
	}
	return nil
}

// HTTPClient builds the client the agent uses for every mother connection
// (push loop and self-update).
//
// The transport is left at Go's default, which means the system trust store
// and nothing else. The agent no longer carries CA_FILE or TLS_SKIP_VERIFY:
// the mother serves plain HTTP, and where TLS is terminated by something in
// front of it that proxy is expected to present a certificate the host already
// trusts. Removing the knobs removes the failure mode where a relaxed setting
// meant for a self-signed mother silently followed the agent to any other host
// it was later pointed at.
func (c Config) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}
