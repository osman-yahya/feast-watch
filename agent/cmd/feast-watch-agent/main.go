package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/osman-yahya/feast-watch/agent"
	"github.com/osman-yahya/feast-watch/agent/collectors"
)

func main() {
	confPath := flag.String("config", "/etc/feast-watch/agent.conf", "config file path")
	flag.Parse()

	cfg, err := agent.LoadConfig(*confPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	reg := collectors.NewRegistry()
	reg.Register(collectors.NewCPU())
	reg.Register(collectors.NewMemory())
	reg.Register(collectors.NewUptime())
	reg.Register(collectors.NewDisk())
	// Service collectors register only when configured; the mother's enabled
	// list decides whether they actually run.
	if cfg.CentrifugoAPIURL != "" {
		reg.Register(collectors.NewCentrifugo(cfg.CentrifugoAPIURL, cfg.CentrifugoAPIKey, cfg.CentrifugoConnsMax))
	}
	if cfg.DragonflyAddr != "" {
		reg.Register(collectors.NewDragonfly(cfg.DragonflyAddr))
	}
	if cfg.PostgresDSN != "" {
		reg.Register(collectors.NewPostgres(cfg.PostgresDSN))
	}
	if cfg.K8sAPIURL != "" {
		reg.Register(collectors.NewK8s(cfg.K8sAPIURL, cfg.K8sToken))
	}

	loop := agent.NewLoop(cfg, reg)
	loop.Run(context.Background(), func(desired string) {
		slog.Info("self-update requested", "desired", desired)
		if err := agent.SelfUpdate(cfg, desired, os.Exit); err != nil {
			slog.Error("self-update failed", "err", err)
		}
	})
}
