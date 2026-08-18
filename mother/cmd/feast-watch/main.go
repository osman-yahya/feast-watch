package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/api"
	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/store"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
)

// defaultReleasePoll is how often the mother re-reads the published releases.
// Unauthenticated GitHub allows 60 requests an hour per IP and does not count
// a conditional request answered 304, so this costs ~12 billed requests an
// hour in the worst case and none in the common one.
const defaultReleasePoll = 5 * time.Minute

func releasePollInterval() time.Duration {
	raw := os.Getenv("FW_RELEASE_POLL_INTERVAL")
	if raw == "" {
		return defaultReleasePoll
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("FW_RELEASE_POLL_INTERVAL is not a positive duration; using the default",
			"value", raw, "default", defaultReleasePoll)
		return defaultReleasePoll
	}
	return d
}

// seedBuilds parses FW_AGENT_VERSIONS, "v1.3.0=linux-amd64,linux-arm64;v1.4.0=linux-amd64".
// It exists for a mother that cannot reach api.github.com; anything malformed
// is skipped with a warning rather than refusing to start, because this is a
// convenience input and the fetch is the real source.
func seedBuilds(raw string) []release.Build {
	var out []release.Build
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, platforms, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(version) == "" || strings.TrimSpace(platforms) == "" {
			slog.Warn("FW_AGENT_VERSIONS entry ignored", "entry", entry)
			continue
		}
		out = append(out, release.Build{
			Version:   strings.TrimSpace(version),
			Platforms: strings.Split(strings.TrimSpace(platforms), ","),
		})
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	st, err := store.Open(env("FW_DB_PATH", "/var/lib/feast-watch/mother.db"))
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// The mother serves plain HTTP. Anything that needs TLS terminates it in
	// front and is named here, so agents are told the address that actually
	// answers rather than one this process guesses from its own configuration.
	legacyAddr := os.Getenv("FW_PUBLIC_ADDR")
	if legacyAddr != "" {
		slog.Warn("FW_PUBLIC_ADDR is retired; set FW_PUBLIC_URL with a scheme instead",
			"assuming", "http://"+legacyAddr)
	}
	publicURL, err := mother.PublicURL(os.Getenv("FW_PUBLIC_URL"), legacyAddr)
	if err != nil {
		slog.Error("public url", "err", err)
		os.Exit(1)
	}

	// `feast-watch generate --name=X` — CLI alternative to the panel's Add Server.
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		out, err := mother.RunGenerate(st, publicURL, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(out)
		return
	}

	apiKey := os.Getenv("FW_API_KEY")
	if apiKey == "" {
		slog.Error("FW_API_KEY is required")
		os.Exit(1)
	}
	// The release index is the mother's whole involvement in binary
	// distribution: it names versions, agents download them from GitHub.
	releases := release.NewCache(
		release.NewClient(env("FW_RELEASE_API_URL", sharedrelease.DefaultAPIBaseURL),
			os.Getenv("FW_INCLUDE_PRERELEASES") == "true"),
		time.Now)
	if seed := os.Getenv("FW_AGENT_VERSIONS"); seed != "" {
		// An offline seed for a mother with no route to api.github.com. A
		// successful fetch replaces it.
		releases.Seed(seedBuilds(seed))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go releases.Poll(ctx, releasePollInterval(), func(err error) {
		slog.Error("refresh release index", "err", err)
	})

	a := api.New(st, apiKey, releases)
	a.SetPublicURL(publicURL)

	// No rollup job: ingest folds each push into both tiers as it arrives
	// (store.ApplySamples), so there is nothing to recompute on a timer.
	go func() { // retention hourly
		for range time.Tick(time.Hour) {
			cfg, err := st.GetSettings()
			if err == nil {
				err = st.EnforceRetention(time.Now().Unix(), cfg)
			}
			if err != nil {
				slog.Error("retention", "err", err)
			}
		}
	}()

	listen := env("FW_LISTEN", ":8443")
	slog.Info("mother listening", "addr", listen, "public_url", publicURL)
	err = http.ListenAndServe(listen, a.Handler())
	slog.Error("server stopped", "err", err)
	os.Exit(1)
}
