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

	publicAddr := env("FW_PUBLIC_ADDR", "127.0.0.1:8443")

	// `feast-watch generate --name=X` — CLI alternative to the panel's Add Server.
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		out, err := mother.RunGenerate(st, publicAddr, os.Args[2:])
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
	a.SetPublicAddr(publicAddr)

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
	cert, key := os.Getenv("FW_TLS_CERT"), os.Getenv("FW_TLS_KEY")
	slog.Info("mother listening", "addr", listen, "tls", cert != "")
	if cert != "" {
		err = http.ListenAndServeTLS(listen, cert, key, a.Handler())
	} else {
		err = http.ListenAndServe(listen, a.Handler())
	}
	slog.Error("server stopped", "err", err)
	os.Exit(1)
}
