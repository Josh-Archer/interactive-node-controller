package signals

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAggregatePrecedenceAndFailClosedIdle(t *testing.T) {
	tests := []struct {
		name         string
		observations []Observation
		want         Activity
	}{
		{"game beats interactive", []Observation{{Provider: "process", Activity: ActivityGame}, {Provider: "logind", Activity: ActivityInteractive}}, ActivityGame},
		{"interactive beats idle", []Observation{{Provider: "process", Activity: ActivityIdle}, {Provider: "logind", Activity: ActivityInteractive}}, ActivityInteractive},
		{"unknown prevents idle", []Observation{{Provider: "process", Activity: ActivityIdle}, {Provider: "nvidia", Activity: ActivityUnknown}}, ActivityUnknown},
		{"all idle", []Observation{{Provider: "process", Activity: ActivityIdle}, {Provider: "logind", Activity: ActivityIdle}}, ActivityIdle},
		{"none unknown", nil, ActivityUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := Aggregate(test.observations)
			if got != test.want {
				t.Fatalf("Aggregate() = %q, want %q", got, test.want)
			}
		})
	}
}

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	return f(ctx, command, args...)
}

func TestLogindGraphicalSessionAndDegradation(t *testing.T) {
	provider := LogindProvider{
		Command:        "/usr/bin/loginctl",
		GraphicalTypes: map[string]struct{}{"wayland": {}},
		Timeout:        time.Second,
		Runner: runnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if reflect.DeepEqual(args[:1], []string{"list-sessions"}) {
				return []byte("3 1000 user seat0 tty2\n"), nil
			}
			return []byte("Active=yes\nType=wayland\n"), nil
		}),
	}
	activity, reason, err := provider.Observe(context.Background())
	if err != nil || activity != ActivityInteractive || !strings.Contains(reason, "wayland") {
		t.Fatalf("Observe() = %q, %q, %v", activity, reason, err)
	}

	provider.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("loginctl unavailable")
	})
	if _, _, err := provider.Observe(context.Background()); err == nil {
		t.Fatal("missing logind did not degrade to an error")
	}
}

func TestSessionIDRejectsOptionInjection(t *testing.T) {
	if safeSessionID("--system") || safeSessionID("../3") || safeSessionID("") {
		t.Fatal("unsafe session identifier accepted")
	}
	if !safeSessionID("c2") {
		t.Fatal("normal session identifier rejected")
	}
}

func TestProcessProviderMatchesAndHandlesEmptyProc(t *testing.T) {
	root := t.TempDir()
	pidRoot := filepath.Join(root, "123")
	if err := os.Mkdir(pidRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidRoot, "comm"), []byte("game-bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidRoot, "cmdline"), []byte("/opt/game-bin\x00--safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := ProcessProvider{ProcRoot: root, Names: map[string]struct{}{"game-bin": {}}}
	activity, _, err := provider.Observe(context.Background())
	if err != nil || activity != ActivityGame {
		t.Fatalf("Observe() = %q, %v", activity, err)
	}
	provider.Names = map[string]struct{}{}
	provider.CommandLineContains = []string{"not-present"}
	activity, _, err = provider.Observe(context.Background())
	if err != nil || activity != ActivityIdle {
		t.Fatalf("idle Observe() = %q, %v", activity, err)
	}
	if _, _, err := (ProcessProvider{ProcRoot: t.TempDir()}).Observe(context.Background()); err == nil {
		t.Fatal("empty proc root should degrade to unknown")
	}
}

func TestNVIDIAParserAndProviderDegradation(t *testing.T) {
	values, err := parseUtilization("5\n72\n")
	if err != nil || !reflect.DeepEqual(values, []int{5, 72}) {
		t.Fatalf("parseUtilization() = %#v, %v", values, err)
	}
	for _, malformed := range []string{"", "N/A", "101"} {
		if _, err := parseUtilization(malformed); err == nil {
			t.Fatalf("parseUtilization(%q) unexpectedly succeeded", malformed)
		}
	}
	provider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte("42\n"), nil }),
	}
	activity, _, err := provider.Observe(context.Background())
	if err != nil || activity != ActivityGame {
		t.Fatalf("Observe() = %q, %v", activity, err)
	}
	provider.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("missing") })
	if _, _, err := provider.Observe(context.Background()); err == nil {
		t.Fatal("missing nvidia-smi did not degrade to an error")
	}
}

func TestNVIDIAToleratesNonIntegerReadings(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    []int
		wantErr bool
	}{
		{"single N/A with integer", "5\nN/A\n", []int{5}, false},
		{"leading N/A with integer", "N/A\n72\n", []int{72}, false},
		{"not supported string", "0\n[Not Supported]\n", []int{0}, false},
		{"multiple non-integers with integer", "N/A\nERR!\n42\n", []int{42}, false},
		{"all non-integers degrades to error", "N/A\n[N/A]\n", nil, true},
		{"out of range high", "50\n101\n", nil, true},
		{"out of range low", "-1\n50\n", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUtilization(tc.output)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseUtilization(%q) error = %v, wantErr %v", tc.output, err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseUtilization(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}

	idleProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("0\nN/A\n"), nil
		}),
	}
	activity, reason, err := idleProvider.Observe(context.Background())
	if err != nil || activity != ActivityIdle {
		t.Fatalf("idleProvider.Observe() = %q, %q, %v", activity, reason, err)
	}

	obs := []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: reason},
		{Provider: "process", Activity: ActivityIdle, Reason: "no target processes"},
	}
	aggActivity, _ := Aggregate(obs)
	if aggActivity != ActivityIdle {
		t.Fatalf("Aggregate() = %q, want %q", aggActivity, ActivityIdle)
	}

	gameProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("85\nN/A\n"), nil
		}),
	}
	activity, reason, err = gameProvider.Observe(context.Background())
	if err != nil || activity != ActivityGame {
		t.Fatalf("gameProvider.Observe() = %q, %q, %v", activity, reason, err)
	}

	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: reason},
		{Provider: "process", Activity: ActivityIdle, Reason: "no target processes"},
	}
	aggActivity, _ = Aggregate(obs)
	if aggActivity != ActivityGame {
		t.Fatalf("Aggregate() = %q, want %q", aggActivity, ActivityGame)
	}
}

type providerStub struct {
	name     string
	activity Activity
	err      error
}

func (p providerStub) Name() string { return p.name }
func (p providerStub) Observe(context.Context) (Activity, string, error) {
	return p.activity, "stub", p.err
}

func TestCollectConvertsProviderErrorsToUnknown(t *testing.T) {
	observations := Collect(context.Background(), []Provider{providerStub{name: "broken", err: errors.New("boom")}})
	if len(observations) != 1 || observations[0].Activity != ActivityUnknown || observations[0].Err == nil {
		t.Fatalf("Collect() = %#v", observations)
	}
}
