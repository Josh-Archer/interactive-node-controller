package reporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestKubernetesReporterPatchesOnlyBoundNodeActivityStatus(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("rotating-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var captured *http.Request
	var payload []byte
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured = request.Clone(request.Context())
		payload, _ = io.ReadAll(request.Body)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}
	cfg := config.KubernetesConfig{
		APIServer: "https://cluster.example:6443",
		Namespace: "interactive-node-controller",
		Name:      "workstation-1",
		TokenFile: tokenFile,
		CAFile:    "/unused/in/injected-client",
		Timeout:   config.Duration{Duration: time.Second},
	}
	reporter, err := NewKubernetesWithClient(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	status := Status{
		State: reporterStateActive(), Activity: v1alpha1.ActivityGame, Reason: "matched",
		ObservedAt: time.Unix(1, 0).UTC(), HeartbeatAt: time.Unix(2, 0).UTC(), Generation: 7, FailClosed: true,
	}
	if err := reporter.Report(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	wantPath := "/apis/availability.interactive-node.io/v1alpha1/namespaces/interactive-node-controller/nodeactivities/workstation-1/status"
	if captured.Method != http.MethodPatch || captured.URL.Path != wantPath {
		t.Fatalf("request = %s %s, want PATCH %s", captured.Method, captured.URL.Path, wantPath)
	}
	if strings.Contains(captured.URL.Path, "/api/v1/nodes") || strings.Contains(string(payload), `"kind":"Node"`) {
		t.Fatalf("Node mutation invariant violated: %s %s", captured.URL.Path, payload)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer rotating-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/merge-patch+json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var patch struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal(payload, &patch); err != nil {
		t.Fatal(err)
	}
	if patch.Status.Generation != 7 || patch.Status.Reason != "matched" || !patch.Status.FailClosed {
		t.Fatalf("payload status = %#v", patch.Status)
	}
}

func TestKubernetesReporterReloadsTokenAndReportsErrors(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		tokens = append(tokens, request.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader("denied")), Header: make(http.Header)}, nil
	})}
	cfg := config.KubernetesConfig{APIServer: "https://cluster.example", Namespace: "ns", Name: "host", TokenFile: tokenFile, CAFile: "/unused", Timeout: config.Duration{Duration: time.Second}}
	reporter, err := NewKubernetesWithClient(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(context.Background(), Status{}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Report() error = %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = reporter.Report(context.Background(), Status{})
	if len(tokens) != 2 || tokens[0] != "Bearer first" || tokens[1] != "Bearer second" {
		t.Fatalf("tokens = %#v", tokens)
	}
}

func reporterStateActive() State { return StateActive }
