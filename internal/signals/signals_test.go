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
	for _, malformed := range []string{"", "garbage", "101"} {
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
		{"bracketed N/A with integer", "15\n[N/A]\n", []int{15}, false},
		{"all non-integers N/A single GPU", "N/A\n", nil, false},
		{"all non-integers [Not Supported] single GPU", "[Not Supported]\n", nil, false},
		{"all non-integers [N/A] single GPU", "[N/A]\n", nil, false},
		{"all non-integers multi-GPU", "N/A\n[N/A]\n[Not Supported]\n", nil, false},
		{"garbage single line degrades to error", "garbage\n", nil, true},
		{"garbage with integer degrades to error", "N/A\nERR!\n42\n", nil, true},
		{"out of range high", "50\n101\n", nil, true},
		{"out of range low", "-1\n50\n", nil, true},
		{"empty string degrades to error", "", nil, true},
		{"whitespace degrades to error", "   \n\n  ", nil, true},
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

	// Mixed valid readings with idle result
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

	// Mixed valid readings with game result
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

	// All-N/A single GPU yields neutral idle observation and aggregates with idle providers to idle
	singleUnavailable := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("N/A\n"), nil
		}),
	}
	activity, reason, err = singleUnavailable.Observe(context.Background())
	if err != nil || activity != ActivityIdle || reason != "gpu utilization unavailable" {
		t.Fatalf("singleUnavailable.Observe() = %q, %q, %v", activity, reason, err)
	}

	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: reason},
		{Provider: "process", Activity: ActivityIdle, Reason: "no target processes"},
	}
	aggActivity, _ = Aggregate(obs)
	if aggActivity != ActivityIdle {
		t.Fatalf("Aggregate() with all-N/A single GPU = %q, want %q", aggActivity, ActivityIdle)
	}

	// All-unavailable multi-GPU yields neutral idle observation
	multiUnavailable := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("N/A\n[Not Supported]\n[N/A]\n"), nil
		}),
	}
	activity, reason, err = multiUnavailable.Observe(context.Background())
	if err != nil || activity != ActivityIdle || reason != "gpu utilization unavailable" {
		t.Fatalf("multiUnavailable.Observe() = %q, %q, %v", activity, reason, err)
	}

	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: reason},
		{Provider: "process", Activity: ActivityIdle, Reason: "no target processes"},
	}
	aggActivity, _ = Aggregate(obs)
	if aggActivity != ActivityIdle {
		t.Fatalf("Aggregate() with all-unavailable multi-GPU = %q, want %q", aggActivity, ActivityIdle)
	}

	// Exec error / non-zero exit degrades to unknown
	execErrProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("command failed: exit status 1")
		}),
	}
	activity, reason, err = execErrProvider.Observe(context.Background())
	if err == nil || activity != ActivityUnknown {
		t.Fatalf("execErrProvider.Observe() = %q, %v, want unknown", activity, err)
	}
	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: err.Error()},
	}
	if agg, _ := Aggregate(obs); agg != ActivityUnknown {
		t.Fatalf("Aggregate() with exec error = %q, want %q", agg, ActivityUnknown)
	}

	// Timeout degrades to unknown
	timeoutProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", Timeout: time.Millisecond,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return nil, context.DeadlineExceeded
		}),
	}
	activity, reason, err = timeoutProvider.Observe(context.Background())
	if err == nil || activity != ActivityUnknown {
		t.Fatalf("timeoutProvider.Observe() = %q, %v, want unknown", activity, err)
	}
	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: err.Error()},
	}
	if agg, _ := Aggregate(obs); agg != ActivityUnknown {
		t.Fatalf("Aggregate() with timeout = %q, want %q", agg, ActivityUnknown)
	}

	// Empty output degrades to unknown
	emptyProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("   \n\n  "), nil
		}),
	}
	activity, reason, err = emptyProvider.Observe(context.Background())
	if err == nil || activity != ActivityUnknown {
		t.Fatalf("emptyProvider.Observe() = %q, %v, want unknown", activity, err)
	}
	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: err.Error()},
	}
	if agg, _ := Aggregate(obs); agg != ActivityUnknown {
		t.Fatalf("Aggregate() with empty output = %q, want %q", agg, ActivityUnknown)
	}

	// Garbage output degrades to unknown
	garbageProvider := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", Timeout: time.Second,
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("corrupted_output\n"), nil
		}),
	}
	activity, reason, err = garbageProvider.Observe(context.Background())
	if err == nil || activity != ActivityUnknown {
		t.Fatalf("garbageProvider.Observe() = %q, %v, want unknown", activity, err)
	}
	obs = []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: activity, Reason: err.Error()},
	}
	if agg, _ := Aggregate(obs); agg != ActivityUnknown {
		t.Fatalf("Aggregate() with garbage output = %q, want %q", agg, ActivityUnknown)
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
