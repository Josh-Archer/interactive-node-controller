package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, "{}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Agent.FailClosed || !cfg.Signals.Logind.Enabled || cfg.Reporter.Mode != "stdout" {
		t.Fatalf("unsafe defaults: %#v", cfg)
	}
}

func TestLoadRejectsUnknownAndMalformedConfiguration(t *testing.T) {
	tests := map[string]string{
		"unknown field":       "unexpected: true\n",
		"fail open":           "agent:\n  fail_closed: false\n",
		"bad duration":        "agent:\n  sample_interval: soon\n",
		"no providers":        "signals:\n  logind:\n    enabled: false\n",
		"unsafe command":      "signals:\n  logind:\n    command: loginctl\n",
		"empty process match": "signals:\n  logind:\n    enabled: false\n  game_process:\n    enabled: true\n",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, contents))
			if err == nil {
				t.Fatal("Load() unexpectedly succeeded")
			}
		})
	}
}

func TestKubernetesConfigValidation(t *testing.T) {
	valid := KubernetesConfig{
		APIServer: "https://cluster.example:6443",
		Namespace: "interactive-node-controller",
		Name:      "workstation-1",
		TokenFile: "/run/reporter/token",
		CAFile:    "/run/reporter/ca.crt",
		Timeout:   Duration{Defaults().Reporter.Kubernetes.Timeout.Duration},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*KubernetesConfig){
		"HTTP endpoint":  func(c *KubernetesConfig) { c.APIServer = "http://cluster.example" },
		"endpoint path":  func(c *KubernetesConfig) { c.APIServer = "https://cluster.example/api/v1/nodes" },
		"bad namespace":  func(c *KubernetesConfig) { c.Namespace = "../nodes" },
		"relative token": func(c *KubernetesConfig) { c.TokenFile = "token" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
