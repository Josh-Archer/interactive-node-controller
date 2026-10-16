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

func TestLogindLockedSessionReturnsIdle(t *testing.T) {
	provider := LogindProvider{
		Command:        "/usr/bin/loginctl",
		GraphicalTypes: map[string]struct{}{"wayland": {}},
		Timeout:        time.Second,
		Runner: runnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if reflect.DeepEqual(args[:1], []string{"list-sessions"}) {
				return []byte("3 1000 user seat0 tty2\n"), nil
			}
			return []byte("Active=yes\nType=wayland\nLockedHint=yes\n"), nil
		}),
	}
	activity, reason, err := provider.Observe(context.Background())
	if err != nil || activity != ActivityIdle {
		t.Fatalf("expected idle for locked session, got activity=%q reason=%q err=%v", activity, reason, err)
	}

	// Active graphical session with IdleHint=yes but LockedHint=no stays interactive
	provider.Runner = runnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args[:1], []string{"list-sessions"}) {
			return []byte("3 1000 user seat0 tty2\n"), nil
		}
		return []byte("Active=yes\nType=wayland\nIdleHint=yes\nLockedHint=no\n"), nil
	})
	activity, reason, err = provider.Observe(context.Background())
	if err != nil || activity != ActivityInteractive {
		t.Fatalf("expected interactive when only idleHint is true without session lock, got activity=%q reason=%q err=%v", activity, reason, err)
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
		Runner: nvidiaRunner("42\n", "4242, python, 256\n"),
	}
	activity, reason, err := provider.Observe(context.Background())
	if err != nil || activity != ActivityGame || !strings.Contains(reason, "pid 4242") {
		t.Fatalf("Observe() = %q, %q, %v", activity, reason, err)
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
		Runner: nvidiaRunner("0\nN/A\n", ""),
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
		Runner: nvidiaRunner("85\nN/A\n", "99, python, 1024\n"),
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

func nvidiaRunner(utilization, apps string) runnerFunc {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, errors.New("missing nvidia-smi arguments")
		}
		switch args[0] {
		case "--query-gpu=utilization.gpu":
			return []byte(utilization), nil
		case "--query-compute-apps=pid,process_name,used_gpu_memory":
			return []byte(apps), nil
		default:
			return nil, errors.New("unexpected nvidia-smi arguments")
		}
	}
}

func TestNVIDIAComputeAppsClassifiedSeparatelyFromUtilization(t *testing.T) {
	provider := func(utilization, apps string) NVIDIAProvider {
		return NVIDIAProvider{
			Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
			Runner: nvidiaRunner(utilization, apps),
		}
	}

	compositor, reason, err := provider("64\n", "").Observe(context.Background())
	if err != nil || compositor != ActivityIdle || reason != "GPU utilization 64% meets 20% floor without compute apps" {
		t.Fatalf("compositor Observe() = %q, %q, %v", compositor, reason, err)
	}
	obs := []Observation{
		{Provider: "logind", Activity: ActivityIdle, Reason: "no graphical session"},
		{Provider: "nvidia", Activity: compositor, Reason: reason},
		{Provider: "process", Activity: ActivityIdle, Reason: "no target processes"},
	}
	if agg, _ := Aggregate(obs); agg != ActivityIdle {
		t.Fatalf("Aggregate() with compositor utilization = %q, want idle", agg)
	}

	game, reason, err := provider("64\n", "321, python, 2048\n").Observe(context.Background())
	if err != nil || game != ActivityGame || reason != `GPU compute app "python" (pid 321) with utilization 64% meeting 20% floor` {
		t.Fatalf("compute app Observe() = %q, %q, %v", game, reason, err)
	}
	obs[1] = Observation{Provider: "nvidia", Activity: game, Reason: reason}
	if agg, _ := Aggregate(obs); agg != ActivityGame {
		t.Fatalf("Aggregate() with compute app = %q, want game", agg)
	}

	below, reason, err := provider("5\n", "321, python, 2048\n").Observe(context.Background())
	if err != nil || below != ActivityIdle || reason != `GPU compute app "python" (pid 321) with utilization 5% below 20% floor` {
		t.Fatalf("below-floor compute app Observe() = %q, %q, %v", below, reason, err)
	}

	quiet, reason, err := provider("5\n", "\n").Observe(context.Background())
	if err != nil || quiet != ActivityIdle || reason != "GPU utilization 5% below 20% floor" {
		t.Fatalf("quiet Observe() = %q, %q, %v", quiet, reason, err)
	}

	several, reason, err := provider("80\n", "30, high, 100\n20, mid, 500\n10, low, 500\n").Observe(context.Background())
	if err != nil || several != ActivityGame || reason != `GPU compute app "low" (pid 10) and 2 more with utilization 80% meeting 20% floor` {
		t.Fatalf("multi-app Observe() = %q, %q, %v", several, reason, err)
	}

	named, reason, err := provider("40\n", "7, My, Game, [N/A]\n").Observe(context.Background())
	if err != nil || named != ActivityGame || reason != `GPU compute app "My, Game" (pid 7) with utilization 40% meeting 20% floor` {
		t.Fatalf("comma-name Observe() = %q, %q, %v", named, reason, err)
	}

	var calls [][]string
	recording := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(_ context.Context, command string, args ...string) ([]byte, error) {
			if command != "/usr/bin/nvidia-smi" {
				t.Fatalf("command = %q", command)
			}
			calls = append(calls, append([]string(nil), args...))
			if len(args) > 0 && args[0] == "--query-compute-apps=pid,process_name,used_gpu_memory" {
				return []byte("15, train, 64 MiB\n"), nil
			}
			return []byte("55\n"), nil
		}),
	}
	activity, reason, err := recording.Observe(context.Background())
	if err != nil || activity != ActivityGame || !strings.Contains(reason, `pid 15`) {
		t.Fatalf("recording Observe() = %q, %q, %v", activity, reason, err)
	}
	wantCalls := [][]string{
		{"--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"},
		{"--query-compute-apps=pid,process_name,used_gpu_memory", "--format=csv,noheader,nounits"},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("nvidia-smi args = %#v, want %#v", calls, wantCalls)
	}

	brokenApps := NVIDIAProvider{
		Command: "/usr/bin/nvidia-smi", UtilizationFloor: 20, Timeout: time.Second,
		Runner: runnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) > 0 && strings.HasPrefix(args[0], "--query-compute-apps=") {
				return nil, errors.New("compute query failed")
			}
			return []byte("90\n"), nil
		}),
	}
	activity, _, err = brokenApps.Observe(context.Background())
	if err == nil || activity != ActivityUnknown || !strings.Contains(err.Error(), "query NVIDIA compute apps") {
		t.Fatalf("brokenApps.Observe() = %q, %v", activity, err)
	}

	for _, malformed := range []string{"not-an-app\n", "0, python, 10\n", "12, python\n", "12, , 10\n", "12, python, no-mem\n"} {
		malformedProvider := provider("33\n", malformed)
		activity, _, err = malformedProvider.Observe(context.Background())
		if err == nil || activity != ActivityUnknown {
			t.Fatalf("malformed %q Observe() = %q, %v", malformed, activity, err)
		}
	}
}

func TestParseComputeApps(t *testing.T) {
	apps, err := parseComputeApps("  \n")
	if err != nil || apps != nil {
		t.Fatalf("empty parseComputeApps() = %#v, %v", apps, err)
	}
	apps, err = parseComputeApps("321, python, 2048\n15, /opt/bin/train, 64 MiB\n")
	if err != nil || !reflect.DeepEqual(apps, []computeApp{{PID: 321, Name: "python", Memory: 2048}, {PID: 15, Name: "/opt/bin/train", Memory: 64}}) {
		t.Fatalf("parseComputeApps() = %#v, %v", apps, err)
	}
	if _, err := parseComputeApps("nope\n"); err == nil {
		t.Fatal("malformed compute app accepted")
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
