package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/core"
	"github.com/aware/gateway/internal/plugin"
	"github.com/aware/gateway/internal/trace"

	// Built-in plugins (imported for side-effect registration via init())
	"github.com/aware/gateway/plugins/audit"
	"github.com/aware/gateway/plugins/otelgenai"
	"github.com/aware/gateway/plugins/ratelimit"
	"github.com/aware/gateway/plugins/taskrouter"
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to config file")
	flag.Parse()

	// Setup structured logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"listen", cfg.Server.Listen,
		"pools", fmt.Sprintf("%v", poolNames(cfg)),
		"routes", len(cfg.Routes),
	)

	// Initialize tracing
	var traceShutdown trace.ShutdownFunc
	if cfg.Tracing.Enabled {
		traceShutdown, err = trace.Setup(trace.Config{
			Enabled:     cfg.Tracing.Enabled,
			Exporter:    cfg.Tracing.Exporter,
			Endpoint:    cfg.Tracing.Endpoint,
			Insecure:    cfg.Tracing.Insecure,
			ServiceName: cfg.Tracing.ServiceName,
			SampleRate:  cfg.Tracing.SampleRate,
		})
		if err != nil {
			slog.Error("failed to initialize tracing", "error", err)
			os.Exit(1)
		}
	}

	// Pool manager (hot-reload capable)
	pm := core.NewPoolManager()

	// Initial pool creation
	pools := core.CreatePools(cfg)
	pm.SwapAndReturn(pools, cfg)

	// Plugin registry
	registry := plugin.NewRegistry(slog.Default())

	// Register built-in plugins explicitly.
	// Order matters for middleware (lower priority = outermost wrapper).
	// Routers run chain-of-responsibility; audit sinks are fan-out.
	mustRegister(registry, &ratelimit.Plugin{})
	mustRegister(registry, &otelgenai.Plugin{})
	mustRegister(registry, &taskrouter.Router{})
	mustRegister(registry, &audit.Plugin{})

	// Stop channel for background goroutines
	stopChan := make(chan struct{})

	// Initialize plugins
	pctx := &plugin.Context{
		Config:   cfg,
		Logger:   slog.Default(),
		Pools:    pm,
		StopChan: stopChan,
	}
	if err := registry.Init(pctx); err != nil {
		slog.Error("plugin initialization failed", "error", err)
		os.Exit(1)
	}

	// Build router with plugin middleware
	handler := core.BuildRouter(cfg, pm, registry, slog.Default())

	// HTTP server
	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: cfg.Server.Timeout,
		IdleTimeout:  120 * time.Second,
	}

	// Start server
	go func() {
		slog.Info("gateway starting", "listen", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Background: refresh pool health gauges
	gaugeCtx, gaugeCancel := context.WithCancel(context.Background())
	defer gaugeCancel()
	go core.RefreshPoolGauges(gaugeCtx, pm)

	// Signal handling
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)

	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, syscall.SIGINT, syscall.SIGTERM)

	// Main event loop
	for {
		select {
		case sig := <-sighupCh:
			slog.Info("received SIGHUP, reloading config", "signal", sig)
			newCfg, err := config.Load(*configPath)
			if err != nil {
				slog.Error("config reload failed, keeping current config", "error", err)
				continue
			}
			newPools := core.CreatePools(newCfg)
			oldPools := pm.SwapAndReturn(newPools, newCfg)
			for name, p := range oldPools {
				slog.Info("stopping old pool health checks", "pool", name)
				p.Stop()
			}
			// Rebuild router
			newHandler := core.BuildRouter(newCfg, pm, registry, slog.Default())
			srv.Handler = newHandler
			gaugeCancel()
			gaugeCtx, gaugeCancel = context.WithCancel(context.Background())
			go core.RefreshPoolGauges(gaugeCtx, pm)
			slog.Info("config reloaded",
				"pools", fmt.Sprintf("%v", poolNames(newCfg)),
				"routes", len(newCfg.Routes),
			)

		case sig := <-quitCh:
			slog.Info("shutting down", "signal", sig)

			// Close plugins
			if err := registry.Close(); err != nil {
				slog.Error("plugin close error", "error", err)
			}

			// Stop pool health checks
			for name, p := range pm.All() {
				p.Stop()
				slog.Info("stopped pool health checks", "pool", name)
			}

			// Close stop channel
			close(stopChan)

			// Flush traces
			if traceShutdown != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := traceShutdown(shutdownCtx); err != nil {
					slog.Error("trace shutdown error", "error", err)
				}
				cancel()
			}

			// Graceful shutdown
			activeCfg := pm.Config()
			shutdownTimeout := cfg.Server.GracefulShutdown
			if activeCfg != nil && activeCfg.Server.GracefulShutdown > 0 {
				shutdownTimeout = activeCfg.Server.GracefulShutdown
			}
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer shutdownCancel()

			if err := srv.Shutdown(shutdownCtx); err != nil {
				slog.Error("shutdown error", "error", err)
				os.Exit(1)
			}

			gaugeCancel()
			slog.Info("gateway stopped")
			return
		}
	}
}

func mustRegister(reg *plugin.Registry, p plugin.Plugin) {
	if err := reg.Register(p); err != nil {
		slog.Error("failed to register plugin", "name", p.Name(), "error", err)
		os.Exit(1)
	}
}

func poolNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		names = append(names, name)
	}
	return names
}
