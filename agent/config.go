// Package agent implements the feast-watch agent: config, push loop, self-update.
package agent

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
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

	// CAFile, if set, is a PEM file of CA certificates the agent trusts for
	// the mother's TLS connection, in addition to (not instead of) the
	// system trust store — needed when the mother presents a certificate
	// signed by an internal/private CA.
	CAFile string
	// TLSSkipVerify disables TLS certificate verification entirely. It is
	// ignored when CAFile is set (see HTTPClient) and should only be used
	// against non-production mothers with throwaway certs.
	TLSSkipVerify bool
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
		CAFile:           kv["CA_FILE"],
		TLSSkipVerify:    kv["TLS_SKIP_VERIFY"] == "true",
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
	return cfg, nil
}

// HTTPClient builds the client the agent uses for every mother connection
// (push loop and self-update). Trust resolution: CAFile takes precedence and
// extends the system trust store with the internal CA; TLSSkipVerify is
// honored only when no CAFile is set. With neither, the default transport
// (system trust store) applies.
func (c Config) HTTPClient(timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	switch {
	case c.CAFile != "":
		pemBytes, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("CA_FILE %s contains no valid PEM certificates", c.CAFile)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	case c.TLSSkipVerify:
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return client, nil
}
