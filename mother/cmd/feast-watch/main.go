package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/api"
	"github.com/osman-yahya/feast-watch/mother/store"
)

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
	a := api.New(st, apiKey, env("FW_DOWNLOADS_DIR", "/var/lib/feast-watch/downloads"))
	a.SetPublicURL(publicURL)

	go func() { // rollup every 30s over the last 10 minutes (idempotent REPLACE)
		for range time.Tick(30 * time.Second) {
			if err := st.RollupSince(time.Now().Unix() - 600); err != nil {
				slog.Error("rollup", "err", err)
			}
		}
	}()
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
