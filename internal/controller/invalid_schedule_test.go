package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type namedErrEvictor struct {
	names   []types.NamespacedName
	options []metav1.DeleteOptions
	deny    map[string]error
}

func (e *namedErrEvictor) Evict(_ context.Context, namespace, name string, options metav1.DeleteOptions) error {
	e.names = append(e.names, types.NamespacedName{Namespace: namespace, Name: name})
	e.options = append(e.options, options)
	if e.deny != nil {
		if err := e.deny[name]; err != nil {
			return err
		}
	}
	return nil
}

type scheduleFixture struct {
	t        *testing.T
	r        *NodeActivityReconciler
	c        client.Client
	activity *availabilityv1alpha1.NodeActivity
	node     *corev1.Node
	now      time.Time
}

func newIdleFixture(t *testing.T, now time.Time, objects ...client.Object) *scheduleFixture {
	t.Helper()
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	all := append([]client.Object{activity, node}, objects...)
	c := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).
		WithStatusSubresource(&availabilityv1alpha1.NodeActivity{}).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithObjects(all...).
		Build()
	r := &NodeActivityReconciler{Client: c, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	return &scheduleFixture{t: t, r: r, c: c, activity: activity, node: node, now: now}
}

func (f *scheduleFixture) reconcile(activity *availabilityv1alpha1.NodeActivity) {
	f.t.Helper()
	if activity == nil {
		activity = f.activity
	}
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: activity.Namespace, Name: activity.Name}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *scheduleFixture) refresh() {
	f.t.Helper()
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: f.node.Name}, f.node); err != nil {
		f.t.Fatal(err)
	}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: f.activity.Namespace, Name: f.activity.Name}, f.activity); err != nil {
		f.t.Fatal(err)
	}
}

func (f *scheduleFixture) write(name namespaceName, mutate func(*availabilityv1alpha1.NodeActivity)) *availabilityv1alpha1.NodeActivity {
	f.t.Helper()
	if name.namespace == "" {
		name = namespaceName{namespace: f.activity.Namespace, name: f.activity.Name}
	}
	key := types.NamespacedName{Namespace: name.namespace, Name: name.name}
	current := &availabilityv1alpha1.NodeActivity{}
	if err := f.c.Get(context.Background(), key, current); err != nil {
		f.t.Fatal(err)
	}
	mutate(current)
	updatedStatus := current.Status
	if err := f.c.Update(context.Background(), current); err != nil {
		f.t.Fatal(err)
	}
	if err := f.c.Get(context.Background(), key, current); err != nil {
		f.t.Fatal(err)
	}
	current.Status = updatedStatus
	if err := f.c.Status().Update(context.Background(), current); err != nil {
		f.t.Fatal(err)
	}
	return current
}

type namespaceName struct {
	namespace string
	name      string
}

func (f *scheduleFixture) assertUntainted() {
	f.t.Helper()
	f.refresh()
	if len(f.node.Spec.Taints) != 0 {
		f.t.Fatalf("taints = %#v", f.node.Spec.Taints)
	}
	if f.node.Labels[SchedulableNodeLabel] != "true" {
		f.t.Fatalf("schedulable label = %#v", f.node.Labels)
	}
}

func (f *scheduleFixture) assertTaint(value string, effect corev1.TaintEffect) {
	f.t.Helper()
	f.refresh()
	if !hasTaint(f.node, f.r.Policy.Key, value, effect) {
		f.t.Fatalf("taints = %#v, want %s %s", f.node.Spec.Taints, value, effect)
	}
	if _, exists := f.node.Labels[SchedulableNodeLabel]; exists {
		f.t.Fatalf("schedulable label remained: %#v", f.node.Labels)
	}
}

func (f *scheduleFixture) assertInvalid(substrings ...string) {
	f.t.Helper()
	f.refresh()
	condition := meta.FindStatusCondition(f.activity.Status.Conditions, InvalidScheduleCondition)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "InvalidSchedule" {
		f.t.Fatalf("condition = %#v", condition)
	}
	for _, want := range append(substrings, "scheduled windows were not applied", "live activity and heartbeat protection still apply") {
		if !strings.Contains(condition.Message, want) {
			f.t.Fatalf("message %q missing %q", condition.Message, want)
		}
	}
}

func (f *scheduleFixture) assertScheduleCleared() {
	f.t.Helper()
	f.refresh()
	condition := meta.FindStatusCondition(f.activity.Status.Conditions, InvalidScheduleCondition)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ScheduleValid" {
		f.t.Fatalf("condition = %#v", condition)
	}
	applied := meta.FindStatusCondition(f.activity.Status.Conditions, TaintAppliedCondition)
	if applied == nil || applied.Reason != "TaintReconciled" {
		f.t.Fatalf("taint condition = %#v", applied)
	}
}

func TestInvalidScheduleProtectsActiveAndStaleFromIdleNode(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		lead      string
		windows   []availabilityv1alpha1.AvailabilityWindow
		state     availabilityv1alpha1.State
		activity  availabilityv1alpha1.Activity
		heartbeat string
		value     string
		effect    corev1.TaintEffect
		message   string
	}{
		{
			name: "active-game-invalid-cron", heartbeat: "fresh",
			windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "not a cron", Duration: "1h", State: "game"}},
			state:   availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame,
			value: testPolicy().ActiveValue, effect: corev1.TaintEffectNoSchedule, message: `spec.windows[0].schedule "not a cron"`,
		},
		{
			name: "active-interactive-invalid-zone", heartbeat: "fresh",
			windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "Not/AZone", State: "interactive"}},
			state:   availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityInteractive,
			value: testPolicy().InteractiveValue, effect: corev1.TaintEffectPreferNoSchedule, message: `spec.windows[0].timeZone "Not/AZone"`,
		},
		{
			name: "stale-heartbeat-nonpositive-duration", heartbeat: "stale",
			windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "0s", State: "game"}},
			state:   availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle,
			value: testPolicy().FailClosedValue, effect: corev1.TaintEffectNoSchedule, message: `spec.windows[0].duration "0s"`,
		},
		{
			name: "missing-heartbeat-negative-lead", heartbeat: "missing", lead: "-5m",
			windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", State: "interactive"}},
			state:   availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle,
			value: testPolicy().FailClosedValue, effect: corev1.TaintEffectNoSchedule, message: `spec.leadTime "-5m"`,
		},
		{
			name: "reported-stale-invalid-state", heartbeat: "fresh",
			windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", State: "gaming"}},
			state:   availabilityv1alpha1.StateStale, activity: availabilityv1alpha1.ActivityUnknown,
			value: testPolicy().FailClosedValue, effect: corev1.TaintEffectNoSchedule, message: `spec.windows[0].state "gaming"`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newIdleFixture(t, now)
			f.reconcile(nil)
			f.assertUntainted()
			if condition := meta.FindStatusCondition(f.activity.Status.Conditions, InvalidScheduleCondition); condition != nil {
				t.Fatalf("idle node already had %#v", condition)
			}
			f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
				activity.Spec.LeadTime = test.lead
				activity.Spec.Windows = test.windows
				activity.Status.State = test.state
				activity.Status.Activity = test.activity
				switch test.heartbeat {
				case "stale":
					activity.Status.HeartbeatAt = now.Add(-2 * time.Minute)
				case "missing":
					activity.Status.HeartbeatAt = time.Time{}
				default:
					activity.Status.HeartbeatAt = now
				}
			})
			f.reconcile(nil)
			f.assertTaint(test.value, test.effect)
			f.assertInvalid(test.message)
			f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
				activity.Spec.LeadTime = ""
				activity.Spec.Windows = nil
				activity.Status.State = availabilityv1alpha1.StateIdle
				activity.Status.Activity = availabilityv1alpha1.ActivityIdle
				activity.Status.HeartbeatAt = now
			})
			f.reconcile(nil)
			f.assertUntainted()
			f.assertScheduleCleared()
		})
	}
}

func TestFreshIdleInvalidScheduleFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	f := newIdleFixture(t, now)
	f.reconcile(nil)
	f.assertUntainted()
	f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{Schedule: "not a cron", Duration: "-1h", TimeZone: "Not/AZone", State: "game"}}
		activity.Status.State = availabilityv1alpha1.StateIdle
		activity.Status.Activity = availabilityv1alpha1.ActivityIdle
		activity.Status.HeartbeatAt = now
	})
	f.reconcile(nil)
	f.assertTaint(testPolicy().FailClosedValue, corev1.TaintEffectNoSchedule)
	f.assertInvalid(`spec.windows[0].schedule "not a cron"`, `spec.windows[0].duration "-1h"`, `spec.windows[0].timeZone "Not/AZone"`)
	applied := meta.FindStatusCondition(f.activity.Status.Conditions, TaintAppliedCondition)
	if applied == nil || applied.Status != metav1.ConditionTrue || applied.Reason != "TaintReconciled" || !strings.Contains(applied.Message, "availability schedule is invalid") {
		t.Fatalf("taint condition = %#v", applied)
	}
}

func TestValidScheduleDoesNotSetInvalidCondition(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 30, 0, 0, time.UTC)
	f := newIdleFixture(t, now)
	f.reconcile(nil)
	f.assertUntainted()
	f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Spec.LeadTime = "30m"
		activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}}
		activity.Status.HeartbeatAt = now
	})
	f.reconcile(nil)
	f.assertTaint(testPolicy().ActiveValue, corev1.TaintEffectNoSchedule)
	if condition := meta.FindStatusCondition(f.activity.Status.Conditions, InvalidScheduleCondition); condition != nil {
		t.Fatalf("valid schedule condition = %#v", condition)
	}
	applied := meta.FindStatusCondition(f.activity.Status.Conditions, TaintAppliedCondition)
	if applied == nil || applied.Reason != "ScheduledWindow" {
		t.Fatalf("taint condition = %#v", applied)
	}
}

func TestInvalidDuplicateEnrollmentCannotBlockProtection(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	t.Run("invalid enrollment does not block the other enrollment", func(t *testing.T) {
		pod := eligiblePod("candidate")
		f := newIdleFixture(t, now, pod)
		evictor := &namedErrEvictor{}
		f.r.Evictor = evictor
		f.r.Eviction = EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 5, RetryBackoff: time.Second}
		f.reconcile(nil)
		f.assertUntainted()

		invalid := &availabilityv1alpha1.NodeActivity{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: "a"},
			Spec: availabilityv1alpha1.NodeActivitySpec{
				NodeName: "desktop",
				Windows:  []availabilityv1alpha1.AvailabilityWindow{{Schedule: "bad cron", Duration: "1h", State: "idle-allowed"}},
			},
		}
		if err := f.c.Create(context.Background(), invalid); err != nil {
			t.Fatal(err)
		}
		invalid.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now}
		if err := f.c.Status().Update(context.Background(), invalid); err != nil {
			t.Fatal(err)
		}
		f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
			activity.Status.State = availabilityv1alpha1.StateActive
			activity.Status.Activity = availabilityv1alpha1.ActivityGame
			activity.Status.HeartbeatAt = now
		})
		f.reconcile(invalid)
		f.assertTaint(testPolicy().ActiveValue, corev1.TaintEffectNoSchedule)
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: invalid.Namespace, Name: invalid.Name}, invalid); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(invalid.Status.Conditions, InvalidScheduleCondition)
		if condition == nil || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, `spec.windows[0].schedule "bad cron"`) {
			t.Fatalf("invalid condition = %#v", condition)
		}
		applied := meta.FindStatusCondition(invalid.Status.Conditions, TaintAppliedCondition)
		if applied == nil || applied.Reason != "DuplicateEnrollment" {
			t.Fatalf("invalid enrollment taint condition = %#v", applied)
		}
		if len(evictor.names) != 0 {
			t.Fatalf("duplicate enrollment evicted %#v", evictor.names)
		}
		f.reconcile(nil)
		f.refresh()
		if condition := meta.FindStatusCondition(f.activity.Status.Conditions, InvalidScheduleCondition); condition != nil && condition.Status == metav1.ConditionTrue {
			t.Fatalf("valid enrollment marked invalid: %#v", condition)
		}
		applied = meta.FindStatusCondition(f.activity.Status.Conditions, TaintAppliedCondition)
		if applied == nil || applied.Reason != "DuplicateEnrollment" {
			t.Fatalf("valid enrollment taint condition = %#v", applied)
		}
		if !hasTaint(f.node, f.r.Policy.Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
			t.Fatalf("protection lost after valid reconcile: %#v", f.node.Spec.Taints)
		}
		if len(evictor.names) != 0 {
			t.Fatalf("duplicate enrollment evicted %#v", evictor.names)
		}
	})

	t.Run("stale invalid enrollment still protects and survives sibling deletion", func(t *testing.T) {
		f := newIdleFixture(t, now)
		f.reconcile(nil)
		f.assertUntainted()
		invalid := &availabilityv1alpha1.NodeActivity{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: "a"},
			Spec: availabilityv1alpha1.NodeActivitySpec{
				NodeName: "desktop",
				Windows:  []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "Not/AZone", State: "game"}},
			},
		}
		if err := f.c.Create(context.Background(), invalid); err != nil {
			t.Fatal(err)
		}
		invalid.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now.Add(-2 * time.Minute)}
		if err := f.c.Status().Update(context.Background(), invalid); err != nil {
			t.Fatal(err)
		}
		f.reconcile(invalid)
		f.assertTaint(testPolicy().FailClosedValue, corev1.TaintEffectNoSchedule)
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: invalid.Namespace, Name: invalid.Name}, invalid); err != nil {
			t.Fatal(err)
		}
		condition := meta.FindStatusCondition(invalid.Status.Conditions, InvalidScheduleCondition)
		if condition == nil || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, "Not/AZone") {
			t.Fatalf("invalid condition = %#v", condition)
		}
		if applied := meta.FindStatusCondition(invalid.Status.Conditions, TaintAppliedCondition); applied == nil || applied.Reason != "DuplicateEnrollment" {
			t.Fatalf("taint condition = %#v", applied)
		}
		f.reconcile(nil)
		if err := f.c.Delete(context.Background(), f.activity); err != nil {
			t.Fatal(err)
		}
		f.reconcile(nil)
		if err := f.c.Get(context.Background(), types.NamespacedName{Name: f.node.Name}, f.node); err != nil {
			t.Fatal(err)
		}
		if !hasTaint(f.node, f.r.Policy.Key, testPolicy().FailClosedValue, corev1.TaintEffectNoSchedule) {
			t.Fatalf("deleting the valid sibling dropped protection: %#v", f.node.Spec.Taints)
		}
		err := f.c.Get(context.Background(), types.NamespacedName{Namespace: f.activity.Namespace, Name: f.activity.Name}, f.activity)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("deleted enrollment err = %v", err)
		}
	})
}

func TestInvalidSchedulePreservesEvictionSafetyGates(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	t.Run("active game keeps opt-in pdb and workload gates", func(t *testing.T) {
		eligible := eligiblePod("eligible")
		notOpted := eligiblePodWithLabels("not-opted", map[string]string{"other": "true"})
		daemon := eligiblePod("daemon")
		daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
		pinned := eligiblePod("pinned")
		pinned.Annotations = map[string]string{PinnedAnnotation: "true"}
		hostpath := eligiblePod("hostpath")
		hostpath.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib"}}}}
		blocked := eligiblePod("pdb-blocked")
		protected := eligiblePod("protected")
		protected.Namespace = "kube-system"
		replicas := int32(2)
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "game", Namespace: "workloads"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}

		f := newIdleFixture(t, now, eligible, notOpted, daemon, pinned, hostpath, blocked, protected, deployment)
		evictor := &namedErrEvictor{deny: map[string]error{"pdb-blocked": apierrors.NewTooManyRequests("PDB blocks eviction", 0)}}
		f.r.Evictor = evictor
		f.r.Eviction = EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 10, RetryBackoff: time.Second}
		f.reconcile(nil)
		f.assertUntainted()
		if len(evictor.names) != 0 {
			t.Fatalf("idle reconcile evicted %#v", evictor.names)
		}
		f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
			activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{Schedule: "not a cron", Duration: "1h", State: "game"}}
			activity.Status.State = availabilityv1alpha1.StateActive
			activity.Status.Activity = availabilityv1alpha1.ActivityGame
			activity.Status.HeartbeatAt = now
		})
		f.reconcile(nil)
		f.assertTaint(testPolicy().ActiveValue, corev1.TaintEffectNoSchedule)
		f.assertInvalid(`spec.windows[0].schedule "not a cron"`)

		attempted := map[string]metav1.DeleteOptions{}
		for i, name := range evictor.names {
			attempted[name.Name] = evictor.options[i]
		}
		for _, skipped := range []string{"not-opted", "daemon", "pinned", "hostpath", "protected"} {
			if _, ok := attempted[skipped]; ok {
				t.Fatalf("%s bypassed a safety gate: %#v", skipped, evictor.names)
			}
		}
		eligibleOptions, ok := attempted["eligible"]
		if !ok {
			t.Fatal("opted-in pod was not evicted")
		}
		if _, ok := attempted["pdb-blocked"]; !ok {
			t.Fatal("PDB-blocked pod was not attempted through the Eviction API")
		}
		if eligibleOptions.Preconditions == nil || eligibleOptions.Preconditions.UID == nil || *eligibleOptions.Preconditions.UID != eligible.UID || eligibleOptions.Preconditions.ResourceVersion == nil || *eligibleOptions.Preconditions.ResourceVersion != eligible.ResourceVersion {
			t.Fatalf("preconditions = %#v", eligibleOptions.Preconditions)
		}
		for _, name := range []string{"eligible", "pdb-blocked", "daemon", "hostpath", "not-opted", "pinned"} {
			if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "workloads", Name: name}, &corev1.Pod{}); err != nil {
				t.Fatalf("pod %s was deleted: %v", name, err)
			}
		}
		eviction := meta.FindStatusCondition(f.activity.Status.Conditions, EvictionCondition)
		if eviction == nil || eviction.Reason != "EvictionBlocked" || !strings.Contains(eviction.Message, "pdb-blocked") {
			t.Fatalf("eviction condition = %#v", eviction)
		}
		gotDeployment := &appsv1.Deployment{}
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}, gotDeployment); err != nil {
			t.Fatal(err)
		}
		if gotDeployment.Spec.Replicas == nil || *gotDeployment.Spec.Replicas != replicas {
			t.Fatalf("deployment replicas = %#v", gotDeployment.Spec.Replicas)
		}
	})

	t.Run("stale heartbeat evicts only lossy opt-in pods", func(t *testing.T) {
		standard := eligiblePod("standard")
		lossy := eligiblePod("lossy")
		lossy.Annotations = map[string]string{AllowLossyEvictionAnnotation: "true"}
		lossyEmpty := eligiblePodWithLabels("lossy-empty", map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
		lossyEmpty.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		lossyHost := eligiblePodWithLabels("lossy-host", map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
		lossyHost.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}}}
		lossyDaemon := eligiblePodWithLabels("lossy-daemon", map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
		lossyDaemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
		lossyPinned := eligiblePodWithLabels("lossy-pinned", map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
		lossyPinned.Annotations = map[string]string{PinnedAnnotation: "true"}

		f := newIdleFixture(t, now, standard, lossy, lossyEmpty, lossyHost, lossyDaemon, lossyPinned)
		evictor := &namedErrEvictor{}
		f.r.Evictor = evictor
		f.r.Eviction = EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 10, RetryBackoff: time.Second}
		f.reconcile(nil)
		f.assertUntainted()
		f.write(namespaceName{}, func(activity *availabilityv1alpha1.NodeActivity) {
			activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "Not/AZone", State: "game"}}
			activity.Status.State = availabilityv1alpha1.StateIdle
			activity.Status.Activity = availabilityv1alpha1.ActivityIdle
			activity.Status.HeartbeatAt = time.Time{}
		})
		f.reconcile(nil)
		f.assertTaint(testPolicy().FailClosedValue, corev1.TaintEffectNoSchedule)
		f.assertInvalid(`spec.windows[0].timeZone "Not/AZone"`)
		attempted := map[string]bool{}
		for _, name := range evictor.names {
			attempted[name.Name] = true
		}
		if !attempted["lossy"] || !attempted["lossy-empty"] {
			t.Fatalf("lossy opt-in pods were not evicted: %#v", evictor.names)
		}
		for _, skipped := range []string{"standard", "lossy-host", "lossy-daemon", "lossy-pinned"} {
			if attempted[skipped] {
				t.Fatalf("%s bypassed a stale-heartbeat safety gate: %#v", skipped, evictor.names)
			}
		}
		for _, pod := range []*corev1.Pod{standard, lossy, lossyEmpty, lossyHost, lossyDaemon, lossyPinned} {
			if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &corev1.Pod{}); err != nil {
				t.Fatalf("pod %s was deleted: %v", pod.Name, err)
			}
		}
	})
}
