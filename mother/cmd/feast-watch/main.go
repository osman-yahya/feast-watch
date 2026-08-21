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
	"syscall"
	"time"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/api"
	"github.com/osman-yahya/feast-watch/mother/build"
	"github.com/osman-yahya/feast-watch/mother/release"
	"github.com/osman-yahya/feast-watch/mother/selfupdate"
	"github.com/osman-yahya/feast-watch/mother/store"
	sharedrelease "github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/version"
)

// defaultReleasePoll is how often the mother re-reads its build catalogue.
// A read is a scan of a handful of directories on local disk, so the interval
// is about how soon a freshly built version appears in the panel, not about
// what the read costs.
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// The deployment's own configuration, for the runs systemd did not start.
	// Under the unit this changes nothing — EnvironmentFile= has already set
	// every key and set values are never replaced — but `feast-watch build`
	// typed by an operator otherwise saw none of it, and would fetch source
	// over the network on a host whose env file names a checkout.
	if err := mother.LoadEnvFile(env("FW_ENV_FILE", mother.DefaultEnvFile)); err != nil {
		slog.Error("read the mother's env file", "err", err)
		os.Exit(1)
	}

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

	// Where this mother's builds live. Read before the CLI dispatch because
	// `feast-watch build` writes here and the server reads here, and the two
	// must agree without either being told twice.
	//
	// Every mother compiles the fleet's binaries and serves them. That is the
	// arrangement rather than a mode of it, because the fleet these agents run
	// on has no route off its network: an agent that had to fetch from
	// somewhere else would have nowhere to fetch from, and a switch for that
	// would only be a way to configure a fleet that cannot update itself.
	sourceDir := os.Getenv("FW_SOURCE_DIR")
	sourceURL := env("FW_SOURCE_URL", sharedrelease.DefaultSourceRepoURL)
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
		// That fetch is the only thing this project asks of the internet, it
		// asks it of this host alone, and it asks for source rather than
		// binaries: what runs on the fleet is compiled here.
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
	// The catalogue is both halves of what used to be a release host: the index
	// of which versions exist, and the bytes themselves. One directory tree
	// answers both, so the panel cannot offer a version that is not
	// downloadable and a download cannot resolve to something the panel never
	// listed.
	//
	// The cost is named where it is paid: this mother is the only authority for
	// what a version means. Nothing outside it computed the checksum an agent
	// verifies against, and there is no published artifact left to compare a
	// build with. Reproducing a version means having the same source tree.
	catalogue := build.New(buildDir)
	releases := release.NewCache(catalogue, time.Now)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go releases.Poll(ctx, releasePollInterval(), func(err error) {
		slog.Error("read the build catalogue", "err", err)
	})

	// The mother's own rollout. `stop` is the same cancel the signal handler
	// uses, so a staged update leaves through the graceful shutdown path rather
	// than exiting from under an in-flight ingest — this process is the single
	// writer of a SQLite database and has to close it cleanly.
	promotePath := env("FW_MOTHER_PROMOTE_PATH", "/usr/local/sbin/feast-watch-mother-promote")
	updater := selfupdate.New(st, selfupdate.Config{
		DownloadBaseURL: publicURL,
		PromotePath:     promotePath,
		StageDir:        env("FW_MOTHER_STAGE_DIR", filepath.Join(filepath.Dir(dbPath), "update")),
		Platform:        runtime.GOOS + "-" + runtime.GOARCH,
		MaxAttempts:     motherUpdateMaxAttempts,
		Interval:        motherUpdateInterval(),
	}, &http.Client{Timeout: 60 * time.Second}, time.Now, stop)

	a := api.New(st, apiKey, releases)
	a.SetPublicURL(publicURL)

	a.SetBinarySource(catalogue)
	slog.Info("serving this mother's own builds", "catalogue", buildDir,
		"agents_download_from", publicURL)

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
