// Package agent implements the feast-watch agent: config, push loop, self-update.
package agent

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	return cfg, nil
}
