package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/api"
	"github.com/osman-yahya/feast-watch/mother/build"
	"github.com/osman-yahya/feast-watch/mother/mirror"
	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/selfupdate"
	"github.com/osman-yahya/feast-watch/mother/store"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/version"
)

// defaultReleasePoll is how often the mother re-reads the published releases.
// Unauthenticated GitHub allows 60 requests an hour per IP and does not count
// a conditional request answered 304, so this costs ~12 billed requests an
// hour in the worst case and none in the common one.
const defaultReleasePoll = 5 * time.Minute

// defaultMotherUpdateInterval is how often the mother checks its own rollout
// target. One indexed read of a single row against a database this process
// already holds open — cheaper than the hourly retention sweep — and it bounds
// "I pressed update" to well under a minute.
const defaultMotherUpdateInterval = 30 * time.Second

// motherUpdateMaxAttempts bounds how many times one target is tried, counted
// across restarts. Enough to ride out a transient network failure, few enough
// that a genuinely broken target stops within a minute or two and leaves a
// readable reason instead of a silent download-exit-restart loop.
const motherUpdateMaxAttempts = 3

func motherUpdateInterval() time.Duration {
	raw := os.Getenv("FW_MOTHER_UPDATE_INTERVAL")
	if raw == "" {
		return defaultMotherUpdateInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("FW_MOTHER_UPDATE_INTERVAL is not a positive duration; using the default",
			"value", raw, "default", defaultMotherUpdateInterval)
		return defaultMotherUpdateInterval
	}
	return d
}

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
	dbPath := env("FW_DB_PATH", "/var/lib/feast-watch/mother.db")
	st, err := store.Open(dbPath)
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

	// Where this mother's own builds live, when it makes its own. Read before
	// the CLI dispatch because `feast-watch build` writes here and the server
	// reads here, and they must agree without either being told twice.
	// selfBuild is the whole architecture in one flag: this mother compiles the
	// fleet's binaries and serves them, so nothing has to be built anywhere
	// else and no artifact has to be published for it to fetch. GitHub is left
	// holding the source, and nothing more.
	selfBuild := os.Getenv("FW_SELF_BUILD") == "true"
	sourceDir := os.Getenv("FW_SOURCE_DIR")
	sourceURL := env("FW_SOURCE_URL", sharedrelease.DefaultBaseURL)
	buildDir := env("FW_BUILD_DIR", filepath.Join(filepath.Dir(dbPath), "builds"))

	// `feast-watch build vX.Y.Z` — compile every platform from FW_SOURCE_DIR
	// into the catalogue this mother serves. The one command that needs a Go
	// toolchain on this host, and the price of answering to nothing outside it.
	if len(os.Args) > 1 && os.Args[1] == "build" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: feast-watch build <version>   (source from FW_SOURCE_DIR)")
			os.Exit(2)
		}
		version := os.Args[2]

		// Where the source comes from. A directory when one is named — an
		// operator's checkout, or a tree carried onto a host that can reach
		// nothing — and otherwise the tag's own source archive, fetched here.
		// That fetch is the last thing this project asks of GitHub, and it asks
		// for source rather than binaries: what runs on the fleet is compiled
		// on this host.
		from := sourceDir
		if from == "" {
			tmp, err := os.MkdirTemp("", "feast-watch-src-")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			defer os.RemoveAll(tmp)
			fmt.Printf("fetching source for %s from %s\n", version, sourceURL)
			if err := build.FetchSource(context.Background(), &http.Client{Timeout: 5 * time.Minute},
				sourceURL, version, tmp); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			from = tmp
		}

		fmt.Printf("building %s from %s\n", version, from)
		if err := build.Build(from, buildDir, version); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("built %s into %s\n", version, filepath.Join(buildDir, version))
		return
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
	// What versions exist, and who says so.
	//
	// With FW_SOURCE_DIR this mother is the whole answer: it compiles from that
	// tree and reads its own catalogue, and nothing here talks to GitHub — not
	// for the bytes, not for the tag that names them. The cost is that the
	// mother is then the only authority for what a version means; there is no
	// published artifact left to check a build against.
	//
	// Without it, the index is the published releases, which is the arrangement
	// everything else in this repository was built around.
	var source release.Source
	var binaries api.BinarySource
	if selfBuild {
		local := build.New(buildDir)
		source, binaries = local, local
	} else {
		source = release.NewClient(env("FW_RELEASE_API_URL", sharedrelease.DefaultAPIBaseURL),
			os.Getenv("FW_INCLUDE_PRERELEASES") == "true")
	}
	releases := release.NewCache(source, time.Now)
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

	// The mother's own rollout. `stop` is the same cancel the signal handler
	// uses, so a staged update leaves through the graceful shutdown path rather
	// than exiting from under an in-flight ingest — this process is the single
	// writer of a SQLite database and has to close it cleanly.
	promotePath := env("FW_MOTHER_PROMOTE_PATH", "/usr/local/sbin/feast-watch-mother-promote")
	updater := selfupdate.New(st, selfupdate.Config{
		ReleaseBaseURL: motherReleaseBaseURL(selfBuild, publicURL),
		PromotePath:    promotePath,
		StageDir:       env("FW_MOTHER_STAGE_DIR", filepath.Join(filepath.Dir(dbPath), "update")),
		Platform:       runtime.GOOS + "-" + runtime.GOARCH,
		MaxAttempts:    motherUpdateMaxAttempts,
		Interval:       motherUpdateInterval(),
	}, &http.Client{Timeout: 60 * time.Second}, time.Now, stop)

	a := api.New(st, apiKey, releases)
	a.SetPublicURL(publicURL)

	// Binary mirroring, off unless asked for.
	//
	// Agents fetching straight from GitHub Releases is the better arrangement
	// wherever it works: binary distribution stays off the monitoring path, the
	// mother stores no builds and serves no bytes, and a rollout cannot be
	// blocked by the mother's disk. Turning that on its head is a decision
	// somebody should have made on purpose — on a fleet whose agents have no
	// route to the internet, a rollout they cannot fetch is not a rollout — so
	// it is a switch rather than a default.
	if binaries == nil && os.Getenv("FW_MIRROR_BINARIES") == "true" {
		cacheDir := env("FW_BINARY_CACHE_DIR", filepath.Join(filepath.Dir(dbPath), "binaries"))
		binaries = mirror.New(cacheDir,
			env("FW_RELEASE_BASE_URL", sharedrelease.DefaultBaseURL),
			&http.Client{Timeout: 60 * time.Second})
		slog.Info("mirroring release binaries for agents", "cache", cacheDir,
			"agents_download_from", publicURL)
	}
	if binaries != nil {
		a.SetBinarySource(binaries)
		if selfBuild {
			slog.Info("serving this mother's own builds", "catalogue", buildDir,
				"agents_download_from", publicURL)
		}
	}
	a.SetMotherUpdate(updater)
	if !updater.Supported() {
		// Said once, at boot, because the panel will show `unsupported` with
		// no way to tell whether that is deliberate or a missing file.
		slog.Info("mother self-update is unavailable: no promote helper on this deployment",
			"looked_for", promotePath)
	}
	go updater.Run(ctx, version.Version)
	// The live window is stored in settings but held in memory, so it has to
	// be applied at boot as well as on save — otherwise a restart would run
	// the default until an operator happened to press Save.
	if settings, err := st.GetSettings(); err != nil {
		slog.Error("read settings at boot", "err", err)
	} else {
		a.ApplySettings(settings)
	}

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
	srv := &http.Server{Addr: listen, Handler: a.Handler()}

	// Shut down on the same signal that stops the release poller. Trapping
	// SIGTERM without acting on it is worse than not trapping it: the default
	// action is gone, so `systemctl stop` would wait out its whole timeout and
	// then SIGKILL — mid-write, on a database this process is the only writer
	// of.
	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
		}
	}()

	slog.Info("mother listening", "addr", listen, "public_url", publicURL)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		slog.Info("server stopped")
		return
	}
	slog.Error("server stopped", "err", err)
	os.Exit(1)
}

// motherReleaseBaseURL is where the mother fetches its OWN replacement from.
//
// From itself, when it compiles its own builds. The catalogue is then the only
// place a mother binary of that version exists — nothing was ever published
// anywhere else — so pointing at the release host would offer the panel a
// target that can only ever 404. It goes over its own HTTP surface rather than
// straight to the file so the update travels the exact path the fleet's does:
// if serving is broken, the mother finds that out about its own update instead
// of being the one client that never noticed.
//
// From the release host otherwise, which is where its builds actually are.
func motherReleaseBaseURL(selfBuild bool, publicURL string) string {
	if selfBuild {
		return publicURL
	}
	return env("FW_RELEASE_BASE_URL", sharedrelease.DefaultBaseURL)
}
