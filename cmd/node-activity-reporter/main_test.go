package main

import (
	"testing"

	"github.com/Josh-Archer/interactive-node-controller/internal/config"
	"github.com/Josh-Archer/interactive-node-controller/internal/signals"
)

func TestBuildProvidersUsesSelectedActivityMode(t *testing.T) {
	cfg := config.Defaults()
	cfg.Signals.GameProcess.ProcRoot = "/fake-proc"
	providers := buildProviders(cfg)
	if len(providers) != 1 || providers[0].Name() != "game-process" {
		t.Fatalf("game mode providers = %#v, want only game-process", providers)
	}
	if _, ok := providers[0].(signals.ProcessProvider); !ok {
		t.Fatalf("game mode provider type = %T, want signals.ProcessProvider", providers[0])
	}

	cfg.Signals.ActivityMode = "session"
	providers = buildProviders(cfg)
	if len(providers) != 1 || providers[0].Name() != "logind" {
		t.Fatalf("session mode providers = %#v, want legacy logind provider", providers)
	}
}
