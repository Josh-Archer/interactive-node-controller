package controller

import (
	"context"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
)

const contractBurstKey = "availability.interactive-node.io/burst"

func contractPods() (standard, lossy *corev1.Pod) {
	standard = eligiblePod("standard")
	lossy = eligiblePodWithLabels("lossy", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	lossy.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	return standard, lossy
}

func applyContractTaints(node *corev1.Node, desired *corev1.Taint, burstValue string, burst bool) {
	node.Spec.Taints = nil
	if desired != nil {
		node.Spec.Taints = append(node.Spec.Taints, *desired)
	}
	if burst {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
			Key: contractBurstKey, Value: burstValue, Effect: corev1.TaintEffectNoSchedule,
		})
	}
}

func assertEvicted(t *testing.T, evictor *recordingEvictor, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, name := range evictor.names {
		got[name.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("evicted %#v, want %v", evictor.names, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("evicted %#v, want %v", evictor.names, want)
		}
	}
}

func assertPreconditions(t *testing.T, evictor *recordingEvictor, pods ...*corev1.Pod) {
	t.Helper()
	byName := map[string]*corev1.Pod{}
	for _, pod := range pods {
		byName[pod.Name] = pod
	}
	if len(evictor.options) != len(evictor.names) {
		t.Fatalf("options=%#v names=%#v", evictor.options, evictor.names)
	}
	for i, name := range evictor.names {
		pod := byName[name.Name]
		if pod == nil {
			t.Fatalf("unexpected eviction %s", name.Name)
		}
		pre := evictor.options[i].Preconditions
		if pre == nil || pre.UID == nil || *pre.UID != pod.UID || pre.ResourceVersion == nil || *pre.ResourceVersion != pod.ResourceVersion {
			t.Fatalf("preconditions for %s = %#v, want UID %q version %q", pod.Name, pre, pod.UID, pod.ResourceVersion)
		}
	}
}

func TestEvictionSafetyContractMatrix(t *testing.T) {
	failClosed := func(r *NodeActivityReconciler) *corev1.Taint {
		return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.FailClosedValue, Effect: corev1.TaintEffectNoSchedule}
	}
	active := func(r *NodeActivityReconciler) *corev1.Taint {
		return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.ActiveValue, Effect: corev1.TaintEffectNoSchedule}
	}
	interactive := func(r *NodeActivityReconciler) *corev1.Taint {
		return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}
	}
	none := func(*NodeActivityReconciler) *corev1.Taint { return nil }

	tests := []struct {
		name           string
		state          availabilityv1alpha1.State
		activity       availabilityv1alpha1.Activity
		desired        func(*NodeActivityReconciler) *corev1.Taint
		burst          bool
		zeroHeartbeat  bool
		staleHeartbeat bool
		failClosedOff  bool
		want           []string
	}{
		{name: "fresh idle ignores leftover burst", state: availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle, desired: none, burst: true},
		{name: "fresh interactive", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityInteractive, desired: interactive, want: []string{"lossy"}},
		{name: "fresh interactive with burst does not evict standard", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityInteractive, desired: interactive, burst: true, want: []string{"lossy"}},
		{name: "fresh game", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame, desired: active, want: []string{"standard", "lossy"}},
		{name: "fresh game with burst", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame, desired: active, burst: true, want: []string{"standard", "lossy"}},
		{name: "fresh unknown with burst fence", state: availabilityv1alpha1.StateUnknown, activity: availabilityv1alpha1.ActivityUnknown, desired: failClosed, burst: true},
		{name: "fresh unknown without burst", state: availabilityv1alpha1.StateUnknown, activity: availabilityv1alpha1.ActivityUnknown, desired: failClosed},
		{name: "fresh unknown ignores burst when fail-closed is disabled", state: availabilityv1alpha1.StateUnknown, activity: availabilityv1alpha1.ActivityUnknown, desired: none, burst: true, failClosedOff: true},
		{name: "expired heartbeat evicts lossy only", state: availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle, desired: failClosed, staleHeartbeat: true, want: []string{"lossy"}},
		{name: "expired heartbeat on last game report evicts lossy only", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame, desired: failClosed, staleHeartbeat: true, want: []string{"lossy"}},
		{name: "stale heartbeat does not let a lingering active taint evict standard", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame, desired: active, staleHeartbeat: true, want: []string{"lossy"}},
		{name: "reporter stale", state: availabilityv1alpha1.StateStale, activity: availabilityv1alpha1.ActivityIdle, desired: failClosed, want: []string{"lossy"}},
		{name: "missing heartbeat is not a fresh unknown debounce", state: availabilityv1alpha1.StateUnknown, activity: availabilityv1alpha1.ActivityUnknown, desired: failClosed, zeroHeartbeat: true, burst: true, want: []string{"lossy"}},
		{name: "active unsupported activity is lossy, not fresh unknown", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityUnknown, desired: failClosed, burst: true, want: []string{"lossy"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			standard, lossy := contractPods()
			r, activity, node, evictor := evictionFixture(t, test.state, test.activity, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
			if test.failClosedOff {
				r.Policy.FailClosed = false
			}
			if test.burst {
				r.Policy.BurstTaintKey = contractBurstKey
			}
			if test.zeroHeartbeat {
				activity.Status.HeartbeatAt = time.Time{}
			}
			if test.staleHeartbeat {
				r.Clock = clocktesting.NewFakeClock(activity.Status.HeartbeatAt.Add(2 * r.Policy.StaleAfter))
			}
			desired := test.desired(r)
			applyContractTaints(node, desired, r.Policy.FailClosedValue, test.burst)
			summary, err := r.reconcileEvictions(context.Background(), activity, node, desired)
			if err != nil {
				t.Fatal(err)
			}
			assertEvicted(t, evictor, test.want...)
			assertPreconditions(t, evictor, standard, lossy)
			if summary.evicted != len(test.want) || summary.attempted != len(test.want) {
				t.Fatalf("summary=%#v", summary)
			}
		})
	}
}

func TestScheduledAndHoldEvictionContract(t *testing.T) {
	tests := []struct {
		name   string
		window string
		burst  bool
		want   []string
	}{
		{name: "scheduled game without burst", window: "game"},
		{name: "scheduled game with burst", window: "game", burst: true, want: []string{"lossy"}},
		{name: "scheduled interactive without burst", window: "interactive", want: []string{"lossy"}},
		{name: "scheduled interactive with burst", window: "interactive", burst: true, want: []string{"lossy"}},
		{name: "scheduled idle-allowed ignores burst fence", window: "idle-allowed", burst: true},
		{name: "hold evicts lossy only", want: []string{"lossy"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			standard, lossy := contractPods()
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
			if test.burst {
				r.Policy.BurstTaintKey = contractBurstKey
			}
			if test.window == "" {
				activity.Annotations = map[string]string{
					HoldAnnotation:      "2h",
					holdStartAnnotation: activity.Status.HeartbeatAt.UTC().Format(time.RFC3339Nano),
				}
			} else {
				activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{
					Schedule: "0 20 * * *",
					Duration: "2h",
					TimeZone: "UTC",
					State:    test.window,
				}}
			}
			desired, _, _, _, _, err := r.desiredTaint(activity, activity.Status.HeartbeatAt)
			if err != nil {
				t.Fatal(err)
			}
			applyContractTaints(node, desired, r.Policy.FailClosedValue, test.burst)
			if _, err := r.reconcileEvictions(context.Background(), activity, node, desired); err != nil {
				t.Fatal(err)
			}
			assertEvicted(t, evictor, test.want...)
			assertPreconditions(t, evictor, standard, lossy)
		})
	}
}

func TestFreshUnknownBurstFenceDoesNotEvictOnReconcile(t *testing.T) {
	standard, lossy := contractPods()
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateUnknown, availabilityv1alpha1.ActivityUnknown, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	r.Policy.BurstTaintKey = contractBurstKey
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, updated); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(updated, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("fail-closed taint missing: %#v", updated.Spec.Taints)
	}
	if !hasTaint(updated, contractBurstKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("burst taint missing: %#v", updated.Spec.Taints)
	}
	assertEvicted(t, evictor)
}

func TestScheduledGameReconcileEvictionContract(t *testing.T) {
	for _, burst := range []bool{false, true} {
		name := "without burst"
		if burst {
			name = "with burst"
		}
		t.Run(name, func(t *testing.T) {
			standard, lossy := contractPods()
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
			if burst {
				r.Policy.BurstTaintKey = contractBurstKey
			}
			ctx := context.Background()
			key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}
			stored := &availabilityv1alpha1.NodeActivity{}
			if err := r.Get(ctx, key, stored); err != nil {
				t.Fatal(err)
			}
			stored.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{
				Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game",
			}}
			if err := r.Update(ctx, stored); err != nil {
				t.Fatal(err)
			}
			stored.Status = availabilityv1alpha1.NodeActivityStatus{
				State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: activity.Status.HeartbeatAt,
			}
			if err := r.Status().Update(ctx, stored); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if burst {
				assertEvicted(t, evictor, "lossy")
				assertPreconditions(t, evictor, standard, lossy)
			} else {
				assertEvicted(t, evictor)
			}
			updated := &corev1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, updated); err != nil {
				t.Fatal(err)
			}
			if !hasTaint(updated, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
				t.Fatalf("scheduled game taint = %#v", updated.Spec.Taints)
			}
			if burst && !hasTaint(updated, contractBurstKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
				t.Fatalf("burst taint = %#v", updated.Spec.Taints)
			}
		})
	}
}

func TestDuplicateEnrollmentSuppressesLossyEviction(t *testing.T) {
	standard, lossy := contractPods()
	r, active, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	r.Policy.BurstTaintKey = contractBurstKey
	r.Clock = clocktesting.NewFakeClock(active.Status.HeartbeatAt)
	ctx := context.Background()
	other := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other"}, Spec: active.Spec}
	if err := r.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	other.Status = availabilityv1alpha1.NodeActivityStatus{
		State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: active.Status.HeartbeatAt,
	}
	if err := r.Status().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: active.Name, Namespace: active.Namespace}}); err != nil {
		t.Fatal(err)
	}
	assertEvicted(t, evictor)
	updated := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, updated); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(updated, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("duplicate enrollment dropped game protection: %#v", updated.Spec.Taints)
	}
}

func TestEvictionVolumeAndGateContract(t *testing.T) {
	emptyDir := func(pod *corev1.Pod) {
		pod.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	}
	hostPath := func(pod *corev1.Pod) {
		pod.Spec.Volumes = []corev1.Volume{{Name: "hp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib"}}}}
	}
	withClaim := func(pod *corev1.Pod, modes []corev1.PersistentVolumeAccessMode, bound bool) []runtime.Object {
		volumeName := ""
		if bound {
			volumeName = pod.Name + "-pv"
		}
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pod.Name + "-pvc"},
			},
		}}
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name + "-pvc", Namespace: pod.Namespace},
			Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes, VolumeName: volumeName},
		}
		if !bound {
			return []runtime.Object{pvc}
		}
		return []runtime.Object{pvc, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volumeName}}}
	}
	claims := func(modes []corev1.PersistentVolumeAccessMode, bound bool) func(standard, lossy *corev1.Pod) []runtime.Object {
		return func(standard, lossy *corev1.Pod) []runtime.Object {
			return append(withClaim(standard, modes, bound), withClaim(lossy, modes, bound)...)
		}
	}

	tests := []struct {
		name                        string
		mutate                      func(standard, lossy *corev1.Pod) []runtime.Object
		want                        []string
		standardReason, lossyReason string
	}{
		{
			name: "emptyDir",
			mutate: func(standard, lossy *corev1.Pod) []runtime.Object {
				emptyDir(standard)
				emptyDir(lossy)
				return nil
			},
			want: []string{"lossy"}, standardReason: "local-emptydir", lossyReason: "eligible",
		},
		{
			name: "hostPath",
			mutate: func(standard, lossy *corev1.Pod) []runtime.Object {
				hostPath(standard)
				hostPath(lossy)
				return nil
			},
			standardReason: "hostpath-volume", lossyReason: "hostpath-volume",
		},
		{name: "rwo", mutate: claims([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, true), standardReason: "pvc-rwo", lossyReason: "pvc-rwo"},
		{name: "rwop", mutate: claims([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, true), standardReason: "pvc-rwo", lossyReason: "pvc-rwo"},
		{name: "rwx", mutate: claims([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, true), want: []string{"standard"}, standardReason: "eligible", lossyReason: "pvc-not-allowed"},
		{name: "rox", mutate: claims([]corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, true), want: []string{"standard"}, standardReason: "eligible", lossyReason: "pvc-not-allowed"},
		{name: "unbound", mutate: claims([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, false), standardReason: "pvc-not-bound", lossyReason: "pvc-not-bound"},
		{
			name: "protected namespace",
			mutate: func(standard, lossy *corev1.Pod) []runtime.Object {
				standard.Namespace = "kube-system"
				lossy.Namespace = "kube-system"
				return nil
			},
			standardReason: "protected-namespace", lossyReason: "protected-namespace",
		},
		{
			name: "critical",
			mutate: func(standard, lossy *corev1.Pod) []runtime.Object {
				priority := int32(systemPriority)
				standard.Spec.Priority = &priority
				lossy.Spec.Priority = &priority
				return nil
			},
			standardReason: "critical-priority", lossyReason: "critical-priority",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			standard, lossy := contractPods()
			extras := test.mutate(standard, lossy)
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy}, extras...)
			summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
			if err != nil {
				t.Fatal(err)
			}
			assertEvicted(t, evictor, test.want...)
			assertPreconditions(t, evictor, standard, lossy)
			if summary.evicted != len(test.want) {
				t.Fatalf("summary=%#v", summary)
			}
			policy := r.Eviction.normalized()
			for _, check := range []struct {
				pod    *corev1.Pod
				reason string
			}{
				{standard, test.standardReason},
				{lossy, test.lossyReason},
			} {
				decision, reason := r.evictionDecision(context.Background(), check.pod, node.Name, policy)
				wantDecision := reason == "eligible"
				if decision != wantDecision || reason != check.reason {
					t.Fatalf("%s decision=%v reason=%q, want decision=%v reason=%q", check.pod.Name, decision, reason, wantDecision, check.reason)
				}
			}
		})
	}
}

func TestEvictionPDBBlocksStandardAndLossy(t *testing.T) {
	standard, lossy := contractPods()
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	evictor.err = apierrors.NewTooManyRequests("pdb blocks eviction", 0)
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.attempted != 2 || summary.blocked != 2 || summary.evicted != 0 || len(evictor.names) != 2 {
		t.Fatalf("summary=%#v names=%#v", summary, evictor.names)
	}
	assertPreconditions(t, evictor, standard, lossy)
}

func TestLossyEvictionKeepsAuditAndExplicitOptIn(t *testing.T) {
	t.Run("audit does not call the eviction API", func(t *testing.T) {
		_, opted := contractPods()
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{opted})
		r.Eviction.Enabled = false
		r.Eviction.Audit = true
		summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		if summary.audited != 1 || summary.attempted != 0 || summary.evicted != 0 || len(evictor.names) != 0 {
			t.Fatalf("summary=%#v names=%#v", summary, evictor.names)
		}
	})
	t.Run("lossy label without evictable label is skipped", func(t *testing.T) {
		pod := eligiblePodWithLabels("lossy-only", map[string]string{AllowLossyEvictionLabel: "true"})
		pod.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive, corev1.TaintEffectPreferNoSchedule, []*corev1.Pod{pod})
		node.Spec.Taints[0].Value = r.Policy.InteractiveValue
		summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		if summary.evicted != 0 || summary.attempted != 0 || len(evictor.names) != 0 || summary.skipped != 1 {
			t.Fatalf("summary=%#v names=%#v", summary, evictor.names)
		}
		decision, reason := r.evictionDecision(context.Background(), pod, node.Name, r.Eviction.normalized())
		if decision || reason != "not-opted-in" {
			t.Fatalf("decision=%v reason=%q", decision, reason)
		}
	})
}
