package controller

import (
	"context"
	"os"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	leadAt := time.Date(2026, 8, 24, 19, 30, 0, 0, loc)
	startAt := time.Date(2026, 8, 24, 20, 0, 0, 0, loc)
	endAt := time.Date(2026, 8, 24, 22, 0, 0, 0, loc)
	nextStart := time.Date(2026, 8, 25, 20, 0, 0, 0, loc)
	tests := []struct {
		name     string
		now      time.Time
		state    string
		active   bool
		wantWait time.Duration
		wantNext time.Time
	}{
		{"before lead time", time.Date(2026, 8, 24, 19, 0, 0, 0, loc), "", false, 30 * time.Minute, startAt},
		{"nanosecond before lead", leadAt.Add(-time.Nanosecond), "", false, time.Nanosecond, startAt},
		{"lead time begins", leadAt, "game", true, 30 * time.Minute, startAt},
		{"window start", startAt, "game", true, 2 * time.Hour, nextStart},
		{"nanosecond before end", endAt.Add(-time.Nanosecond), "game", true, time.Nanosecond, nextStart},
		{"window end is exclusive", endAt, "", false, 21*time.Hour + 30*time.Minute, nextStart},
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
			if !next.Equal(tt.wantNext) || wait != tt.wantWait {
				t.Fatalf("next=%s wait=%s, want next=%s wait=%s", next, wait, tt.wantNext, tt.wantWait)
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

func TestEvaluateWindowsZeroLeadRequeuesAtStart(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "interactive"}}, LeadTime: "0s"}
	now := time.Date(2026, 8, 24, 19, 0, 0, 0, time.UTC)
	got, next, wait, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || wait != time.Hour || !next.Equal(time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("got=%#v next=%s wait=%s", got, next, wait)
	}
}

func TestEvaluateWindowsOverlappingRanksAndLeadBoundaries(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{
		LeadTime: "30m",
		Windows: []availabilityv1alpha1.AvailabilityWindow{
			{Schedule: "0 20 * * *", Duration: "1h", TimeZone: "UTC", State: "game"},
			{Schedule: "0 20 * * *", Duration: "3h", TimeZone: "UTC", State: "interactive"},
		},
	}
	day := func(hour, min int) time.Time {
		return time.Date(2026, 8, 24, hour, min, 0, 0, time.UTC)
	}
	tests := []struct {
		name     string
		now      time.Time
		state    string
		active   bool
		wantWait time.Duration
	}{
		{"before either lead", day(19, 0), "", false, 30 * time.Minute},
		{"game lead begins", day(19, 30), "game", true, 30 * time.Minute},
		{"both open, game outranks longer interactive", day(20, 30), "game", true, 30 * time.Minute},
		{"game end hands off to interactive", day(21, 0), "interactive", true, 2 * time.Hour},
		{"interactive end waits for the next lead", day(23, 0), "", false, 20*time.Hour + 30*time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, wait, err := evaluateWindows(spec, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tt.active || (got != nil && got.state != tt.state) || wait != tt.wantWait {
				t.Fatalf("window=%#v wait=%s, want state=%q active=%v wait=%s", got, wait, tt.state, tt.active, tt.wantWait)
			}
		})
	}
}

func TestEvaluateWindowsLeadAcrossTimeZones(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{
		LeadTime: "30m",
		Windows: []availabilityv1alpha1.AvailabilityWindow{
			{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"},
			{Schedule: "0 15 * * *", Duration: "1h", TimeZone: "America/New_York", State: "interactive"},
		},
	}
	// 15:00 EDT is 19:00 UTC, so its lead (14:30 EDT) is earlier than the UTC game lead.
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	got, next, wait, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || wait != 30*time.Minute || !next.Equal(time.Date(2026, 8, 24, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("window=%#v next=%s wait=%s", got, next, wait)
	}
	got, _, wait, err = evaluateWindows(spec, now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.state != "interactive" || wait != 30*time.Minute {
		t.Fatalf("at New York lead: window=%#v wait=%s", got, wait)
	}
	got, _, wait, err = evaluateWindows(spec, now.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.state != "game" || wait != 30*time.Minute {
		t.Fatalf("at UTC game lead: window=%#v wait=%s", got, wait)
	}
}

func TestEvaluateWindowsFallBackLeadAndFold(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{LeadTime: "30m", Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 1 * * *", Duration: "90m", TimeZone: "America/New_York", State: "interactive"}}}
	tests := []struct {
		name     string
		now      time.Time
		active   bool
		wantWait time.Duration
	}{
		{"before the first lead", time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC), false, 30 * time.Minute},
		{"first lead begins", time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), true, 30 * time.Minute},
		{"before the repeated hour's lead", time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC), true, 15 * time.Minute},
		{"second occurrence is open", time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), true, 30 * time.Minute},
		{"both occurrences have ended", time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), false, 22 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, wait, err := evaluateWindows(spec, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tt.active || wait != tt.wantWait {
				t.Fatalf("window=%#v wait=%s, want active=%v wait=%s", got, wait, tt.active, tt.wantWait)
			}
		})
	}
}

func TestEvaluateWindowsSpringForwardSkipsMissingHour(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spec := availabilityv1alpha1.NodeActivitySpec{LeadTime: "30m", Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 2 * * *", Duration: "1h", TimeZone: "America/New_York", State: "game"}}}
	// 02:30 does not exist on the spring-forward morning. The next lead is 02:00 the following day.
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	got, next, wait, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if got != nil || !next.Equal(wantNext) || wait != 24*time.Hour {
		t.Fatalf("window=%#v next=%s wait=%s, want next=%s wait=24h", got, next, wait, wantNext)
	}

	// 01:30 EST plus a 3h absolute duration lands at 05:30 EDT, crossing the missing hour.
	crossing := availabilityv1alpha1.NodeActivitySpec{LeadTime: "15m", Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 1 * * *", Duration: "3h", TimeZone: "America/New_York", State: "interactive"}}}
	during := time.Date(2026, 3, 8, 3, 30, 0, 0, loc)
	got, _, wait, err = evaluateWindows(crossing, during)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.state != "interactive" || wait != 2*time.Hour || !got.end.Equal(time.Date(2026, 3, 8, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("crossing window=%#v wait=%s", got, wait)
	}
}

func TestEvaluateWindowsLeadLongerThanPeriod(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{LeadTime: "90m", Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 * * * *", Duration: "10m", TimeZone: "UTC", State: "game"}}}
	now := time.Date(2026, 8, 24, 20, 20, 0, 0, time.UTC)
	got, next, wait, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.state != "game" || wait != 10*time.Minute || !next.Equal(time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("window=%#v next=%s wait=%s", got, next, wait)
	}
}

func TestEvaluateWindowsLongDurationStaysBounded(t *testing.T) {
	rare := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 0 * * *", Duration: "240h", TimeZone: "UTC", State: "idle-allowed"}}}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	got, next, wait, err := evaluateWindows(rare, now)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.state != "idle-allowed" || wait != 12*time.Hour || !next.Equal(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("long rare window=%#v next=%s wait=%s", got, next, wait)
	}

	frequent := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "* * * * *", Duration: "8760h", TimeZone: "UTC", State: "game"}}}
	started := time.Now()
	_, _, _, err = evaluateWindows(frequent, now)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("frequent long duration took %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "* * * * *") || !strings.Contains(err.Error(), "8760h") || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestEvaluateWindowsRejectsImpossibleSchedule(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 0 31 2 *", Duration: "1h", TimeZone: "UTC", State: "game"}}}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := evaluateWindows(spec, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "0 0 31 2 *") || !strings.Contains(err.Error(), "no occurrence") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("evaluateWindows did not return for an impossible schedule")
	}
}

func TestScanWindowRejectsNonAdvancingAndTinySteps(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	_, _, _, err := scanWindow("frozen", "1h", "game", frozenSchedule{at: now.Add(-time.Minute)}, now, time.Hour, 0)
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("frozen error = %v", err)
	}
	_, _, _, err = scanWindow("zero", "1h", "game", zeroSchedule{}, now, time.Hour, 0)
	if err == nil || !strings.Contains(err.Error(), "no occurrence") {
		t.Fatalf("zero error = %v", err)
	}

	pacing := &pacingSchedule{}
	started := time.Now()
	_, _, _, err = scanWindow("pacing", "1h", "game", pacing, now, time.Hour, 0)
	if time.Since(started) > 2*time.Second {
		t.Fatal("pacing schedule was not bounded")
	}
	if err == nil || !strings.Contains(err.Error(), "exceeds") || pacing.calls > maxWindowOccurrences+2 {
		t.Fatalf("pacing error=%v calls=%d", err, pacing.calls)
	}
}

func TestReconcileRequeuesAtLeadTimeWithoutReporterEvent(t *testing.T) {
	start := time.Date(2026, 8, 24, 19, 0, 0, 0, time.UTC)
	clock := clocktesting.NewFakeClock(start)
	policy := testPolicy()
	policy.StaleAfter = 48 * time.Hour
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec: availabilityv1alpha1.NodeActivitySpec{
			NodeName: "workstation-1",
			LeadTime: "30m",
			Windows:  []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}},
		},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: start,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: api, Clock: clock, Policy: policy}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: activity.Namespace, Name: activity.Name}}

	assertRequeue := func(want time.Duration, wantTaint bool) {
		t.Helper()
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != want {
			t.Fatalf("requeue = %s, want %s", result.RequeueAfter, want)
		}
		updated := &corev1.Node{}
		if err := api.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated); err != nil {
			t.Fatal(err)
		}
		gotTaint := hasTaint(updated, policy.Key, policy.ActiveValue, corev1.TaintEffectNoSchedule)
		if gotTaint != wantTaint {
			t.Fatalf("taint present = %v, want %v (%#v)", gotTaint, wantTaint, updated.Spec.Taints)
		}
		current := &availabilityv1alpha1.NodeActivity{}
		if err := api.Get(context.Background(), request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		if !current.Status.HeartbeatAt.Equal(start) {
			t.Fatalf("heartbeat changed to %s without a reporter event", current.Status.HeartbeatAt)
		}
	}

	assertRequeue(30*time.Minute, false)
	clock.Step(30 * time.Minute)
	assertRequeue(30*time.Minute, true)
	clock.Step(30 * time.Minute)
	assertRequeue(2*time.Hour, true)
	clock.Step(2 * time.Hour)
	assertRequeue(21*time.Hour+30*time.Minute, false)
}

type frozenSchedule struct{ at time.Time }

func (s frozenSchedule) Next(time.Time) time.Time { return s.at }

type zeroSchedule struct{}

func (zeroSchedule) Next(time.Time) time.Time { return time.Time{} }

// pacingSchedule advances one nanosecond at a time so an uncapped scan cannot finish.
type pacingSchedule struct{ calls int }

func (s *pacingSchedule) Next(after time.Time) time.Time {
	s.calls++
	if s.calls > maxWindowOccurrences+10 {
		return time.Time{}
	}
	return after.Add(time.Nanosecond)
}
