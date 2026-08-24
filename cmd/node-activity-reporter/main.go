package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Josh-Archer/interactive-node-controller/internal/config"
	"github.com/Josh-Archer/interactive-node-controller/internal/engine"
	"github.com/Josh-Archer/interactive-node-controller/internal/reporter"
	"github.com/Josh-Archer/interactive-node-controller/internal/signals"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("host reporter stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/node-activity-reporter/config.yaml", "configuration file")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *check {
		fmt.Println("configuration is valid")
		return nil
	}

	providers := buildProviders(cfg)
	destination, err := buildReporter(cfg)
	if err != nil {
		return err
	}
	evaluator := engine.New(cfg.Agent.StaleAfter.Duration, engine.DebounceSamples{
		Game:        cfg.Agent.Debounce.GameSamples,
		Interactive: cfg.Agent.Debounce.InteractiveSamples,
		Idle:        cfg.Agent.Debounce.IdleSamples,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	slog.Info("host reporter started", "version", version, "providers", providerNames(providers), "reporter", cfg.Reporter.Mode)
	return reportLoop(ctx, cfg, providers, destination, evaluator)
}

func buildProviders(cfg config.Config) []signals.Provider {
	var providers []signals.Provider
	if cfg.Signals.Logind.Enabled {
		graphicalTypes := make(map[string]struct{}, len(cfg.Signals.Logind.GraphicalTypes))
		for _, sessionType := range cfg.Signals.Logind.GraphicalTypes {
			graphicalTypes[strings.ToLower(sessionType)] = struct{}{}
		}
		providers = append(providers, signals.LogindProvider{
			Command:        cfg.Signals.Logind.Command,
			GraphicalTypes: graphicalTypes,
			Timeout:        cfg.Signals.Logind.Timeout.Duration,
			Runner:         signals.ExecRunner{},
		})
	}
	if cfg.Signals.GameProcess.Enabled {
		names := make(map[string]struct{}, len(cfg.Signals.GameProcess.Names))
		for _, name := range cfg.Signals.GameProcess.Names {
			names[name] = struct{}{}
		}
		providers = append(providers, signals.ProcessProvider{
			ProcRoot:            cfg.Signals.GameProcess.ProcRoot,
			Names:               names,
			CommandLineContains: cfg.Signals.GameProcess.CommandLineContains,
		})
	}
	if cfg.Signals.NVIDIA.Enabled {
		providers = append(providers, signals.NVIDIAProvider{
			Command:          cfg.Signals.NVIDIA.Command,
			UtilizationFloor: cfg.Signals.NVIDIA.UtilizationFloor,
			Timeout:          cfg.Signals.NVIDIA.Timeout.Duration,
			Runner:           signals.ExecRunner{},
		})
	}
	return providers
}

func buildReporter(cfg config.Config) (reporter.Reporter, error) {
	if cfg.Reporter.Mode == "stdout" {
		return reporter.NewJSON(os.Stdout), nil
	}
	return reporter.NewKubernetes(cfg.Reporter.Kubernetes)
}

func reportLoop(ctx context.Context, cfg config.Config, providers []signals.Provider, destination reporter.Reporter, evaluator *engine.Evaluator) error {
	ticker := time.NewTicker(cfg.Agent.SampleInterval.Duration)
	defer ticker.Stop()
	lastReport := time.Time{}
	lastGeneration := int64(0)
	for {
		now := time.Now()
		status := evaluator.Next(signals.Collect(ctx, providers), now)
		if lastReport.IsZero() || status.Generation != lastGeneration || now.Sub(lastReport) >= cfg.Agent.HeartbeatInterval.Duration {
			if err := destination.Report(ctx, status); err != nil {
				slog.Error("report NodeActivity status", "error", err)
			} else {
				lastReport = now
				lastGeneration = status.Generation
				slog.Info("reported host state", "state", status.State, "activity", status.Activity, "generation", status.Generation, "reason", status.Reason)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func providerNames(providers []signals.Provider) []string {
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	return names
}
