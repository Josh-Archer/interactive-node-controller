package controller

import (
	"context"
	"os"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestScheduledWindowEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run isolated API-server tests")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 4, 59, 0, 0, time.UTC)
	clock := clocktesting.NewFakeClock(now)
	activity := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "scheduled-window"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "scheduled-desktop", LeadTime: "30m", Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 1 * * *", Duration: "90m", TimeZone: "America/New_York", State: "interactive"}}}}
	if err := api.Create(context.Background(), activity); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: activity.Spec.NodeName}}
	if err := api.Create(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	policy := testPolicy()
	policy.StaleAfter = 8 * time.Hour
	reconciler := &NodeActivityReconciler{Client: api, Clock: clock, Policy: policy}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: activity.Namespace, Name: activity.Name}}
	reconcile := func() *corev1.Node {
		t.Helper()
		current := &availabilityv1alpha1.NodeActivity{}
		if err := api.Get(context.Background(), request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		current.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: clock.Now()}
		if err := api.Status().Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		updated := &corev1.Node{}
		if err := api.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated); err != nil {
			t.Fatal(err)
		}
		return updated
	}
	if got := reconcile(); hasTaint(got, policy.Key, "", corev1.TaintEffectNoSchedule) || hasTaint(got, policy.Key, policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) {
		t.Fatalf("taints before lead time = %#v", got.Spec.Taints)
	}
	clock.Step(time.Minute)
	if got := reconcile(); !hasTaint(got, policy.Key, policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) {
		t.Fatalf("taints at lead-time boundary = %#v", got.Spec.Taints)
	}
	// The fall-back day has two local 01:30 times. The schedule starts at the
	// first occurrence (05:30 UTC), and its duration crosses the repeated hour.
	clock.Step(30 * time.Minute)
	if got := reconcile(); !hasTaint(got, policy.Key, policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) {
		t.Fatalf("taints in first DST fold = %#v", got.Spec.Taints)
	}
	current := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	current.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: clock.Now()}
	if err := api.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	if err := api.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(updated, policy.Key, policy.ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("live game did not supersede window: %#v", updated.Spec.Taints)
	}
	// Cron observes the repeated 01:30 local time as a second occurrence on
	// fall-back day, so the second 90-minute window ends at 08:00 UTC.
	clock.Step(150 * time.Minute)
	if got := reconcile(); hasTaint(got, policy.Key, policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) || hasTaint(got, policy.Key, policy.ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("managed taint remained at the exclusive window end: %#v", got.Spec.Taints)
	}
}

func TestEvaluateWindowsBoundariesAndLeadTime(t *testing.T) {
	loc := time.UTC
	window := availabilityv1alpha1.AvailabilityWindow{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{window}, LeadTime: "30m"}
	tests := []struct {
		name     string
		now      time.Time
		state    string
		active   bool
		wantWait time.Duration
	}{
		{"before lead time", time.Date(2026, 8, 24, 19, 0, 0, 0, loc), "", false, time.Hour},
		{"lead time begins", time.Date(2026, 8, 24, 19, 30, 0, 0, loc), "game", true, 30 * time.Minute},
		{"window start", time.Date(2026, 8, 24, 20, 0, 0, 0, loc), "game", true, 2 * time.Hour},
		{"window end is exclusive", time.Date(2026, 8, 24, 22, 0, 0, 0, loc), "", false, 22 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, next, wait, err := evaluateWindows(spec, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tt.active {
				t.Fatalf("active window = %#v", got)
			}
			if got != nil && got.state != tt.state {
				t.Fatalf("state = %q, want %q", got.state, tt.state)
			}
			if next.IsZero() || wait != tt.wantWait {
				t.Fatalf("next=%s wait=%s, want wait=%s", next, wait, tt.wantWait)
			}
		})
	}
}

func TestEvaluateWindowsUsesIANAZoneAcrossDST(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 1 * * *", Duration: "90m", TimeZone: "America/New_York", State: "interactive"}}}
	// On the fall-back day, the 01:30 start is the first 01:30 occurrence.
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	window, _, _, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if window == nil || window.start.In(time.UTC).Hour() != 5 || window.start.In(time.UTC).Minute() != 30 {
		t.Fatalf("window start = %#v, want 2026-11-01 05:30 UTC", window)
	}
}

func TestDesiredTaintWindowOverridesInteractiveButNotLiveGame(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 30, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{Spec: availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}}}, Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityInteractive, HeartbeatAt: now}}
	r := &NodeActivityReconciler{Policy: testPolicy()}
	taint, _, _, scheduled, _, err := r.desiredTaint(activity, now)
	if err != nil || !scheduled || taint == nil || taint.Value != testPolicy().ActiveValue {
		t.Fatalf("window desired taint=%#v scheduled=%v err=%v", taint, scheduled, err)
	}
	activity.Status.Activity = availabilityv1alpha1.ActivityGame
	taint, _, _, scheduled, _, err = r.desiredTaint(activity, now)
	if err != nil || scheduled || taint == nil || taint.Value != testPolicy().ActiveValue {
		t.Fatalf("live game desired taint=%#v scheduled=%v err=%v", taint, scheduled, err)
	}
}
