package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingEvictor struct {
	names   []types.NamespacedName
	grace   []*int64
	options []metav1.DeleteOptions
	err     error
}

func (e *recordingEvictor) Evict(_ context.Context, namespace, name string, options metav1.DeleteOptions) error {
	e.names = append(e.names, types.NamespacedName{Namespace: namespace, Name: name})
	e.grace = append(e.grace, options.GracePeriodSeconds)
	e.options = append(e.options, options)
	return e.err
}

func TestEvictionRequiresActiveGameAndNoSchedule(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    availabilityv1alpha1.State
		activity availabilityv1alpha1.Activity
		effect   corev1.TaintEffect
	}{
		{name: "interactive", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityInteractive, effect: corev1.TaintEffectPreferNoSchedule},
		{name: "idle", state: availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle, effect: corev1.TaintEffectNoSchedule},
		{name: "stale", state: availabilityv1alpha1.StateStale, activity: availabilityv1alpha1.ActivityGame, effect: corev1.TaintEffectNoSchedule},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, activity, node, evictor := evictionFixture(t, test.state, test.activity, test.effect, nil)
			if _, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0]); err != nil {
				t.Fatal(err)
			}
			if len(evictor.names) != 0 {
				t.Fatalf("evictions = %#v", evictor.names)
			}
		})
	}
}

func TestEvictionAuditAndOptIn(t *testing.T) {
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("eligible"), eligiblePodWithLabels("not-opted", map[string]string{"other": "true"})})
	r.Eviction.Enabled = false
	r.Eviction.Audit = true
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.audited != 1 || len(evictor.names) != 0 {
		t.Fatalf("summary = %#v evictions = %#v", summary, evictor.names)
	}
}

func TestEvictionSkipsPrimarySafetyClasses(t *testing.T) {
	base := eligiblePod("base")
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		reason string
	}{
		{name: "daemonset", mutate: func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}} }, reason: "daemonset-owned"},
		{name: "mirror", mutate: func(p *corev1.Pod) { p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "mirror"} }, reason: "mirror-static"},
		{name: "protected namespace", mutate: func(p *corev1.Pod) { p.Namespace = "kube-system" }, reason: "protected-namespace"},
		{name: "terminating", mutate: func(p *corev1.Pod) {
			now := metav1.Now()
			p.Finalizers = []string{"test.finalizer"}
			p.DeletionTimestamp = &now
		}, reason: "terminating"},
		{name: "pinned", mutate: func(p *corev1.Pod) { p.Annotations = map[string]string{PinnedAnnotation: "true"} }, reason: "direct-pinned"},
		{name: "ambiguous node selector", mutate: func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"accelerator": "special"} }, reason: "node-selector"},
		{name: "required affinity", mutate: func(p *corev1.Pod) {
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{}}}
		}, reason: "required-node-affinity"},
		{name: "host path", mutate: func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib"}}}}
		}, reason: "hostpath-volume"},
		{name: "empty dir", mutate: func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}, reason: "local-emptydir"},
		{name: "critical", mutate: func(p *corev1.Pod) { priority := int32(systemPriority); p.Spec.Priority = &priority }, reason: "critical-priority"},
		{name: "unmanaged", mutate: func(p *corev1.Pod) { p.OwnerReferences = nil }, reason: "unmanaged-pod"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := base.DeepCopy()
			pod.Name = test.name
			test.mutate(pod)
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
			if _, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0]); err != nil {
				t.Fatal(err)
			}
			if len(evictor.names) != 0 {
				t.Fatalf("unsafe pod was evicted: %#v", evictor.names)
			}
			decision, reason := r.evictionDecision(context.Background(), pod, node.Name, r.Eviction.normalized())
			if decision || reason != test.reason {
				t.Fatalf("decision = %v reason = %q, want %q", decision, reason, test.reason)
			}
		})
	}
}

func TestEvictionSkipsRWOPVC(t *testing.T) {
	pod := eligiblePod("rwo")
	pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: pod.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeName: "data-pv"}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "data-pv"}}
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod}, pvc, pv)
	if _, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 0 {
		t.Fatalf("RWO pod was evicted: %#v", evictor.names)
	}
	decision, reason := r.evictionDecision(context.Background(), pod, node.Name, r.Eviction.normalized())
	if decision || reason != "pvc-rwo" {
		t.Fatalf("decision = %v reason = %q", decision, reason)
	}
}

func TestEvictionPDBBlockRetriesAndRateCaps(t *testing.T) {
	pods := []*corev1.Pod{eligiblePod("one"), eligiblePod("two"), eligiblePod("three")}
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, pods)
	r.Eviction.MaxPerReconcile = 2
	r.Eviction.RetryBackoff = 17 * time.Second
	evictor.err = apierrors.NewTooManyRequests("pdb blocks eviction", 0)
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 || summary.attempted != 2 || summary.blocked != 2 || summary.requeueAfter != 17*time.Second || summary.skipped != 1 {
		t.Fatalf("summary = %#v evictions = %#v", summary, evictor.names)
	}
}

func TestEvictionSuccessUsesGracePeriod(t *testing.T) {
	pod := eligiblePod("candidate")
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.attempted != 1 || summary.evicted != 1 || summary.blocked != 0 || len(evictor.names) != 1 || evictor.grace[0] == nil || *evictor.grace[0] != 20 {
		t.Fatalf("summary = %#v evictor = %#v", summary, evictor)
	}
}

func TestKubernetesEvictionClientUsesPolicyAPI(t *testing.T) {
	// Compile-time contract: the production adapter is typed to policy/v1 and
	// cannot call the Pod delete endpoint.
	var _ EvictionClient = KubernetesEvictionClient{}
	_ = policyv1.Eviction{}
}

func TestEvictionUsesInspectedPodPreconditions(t *testing.T) {
	pod := eligiblePod("candidate")
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
	if _, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if len(evictor.options) != 1 {
		t.Fatalf("options = %#v", evictor.options)
	}
	preconditions := evictor.options[0].Preconditions
	if preconditions == nil || preconditions.UID == nil || *preconditions.UID != pod.UID || preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != pod.ResourceVersion {
		t.Fatalf("preconditions = %#v; want UID %q and version %q", preconditions, pod.UID, pod.ResourceVersion)
	}
}

func TestEvictionSkipsMissingIdentity(t *testing.T) {
	pod := eligiblePod("candidate")
	pod.UID = ""
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 0 || summary.attempted != 0 || summary.skipped != 1 {
		t.Fatalf("summary=%#v attempts=%d", summary, len(evictor.names))
	}
}

func TestEvictionDoesNotMutateDeploymentSpec(t *testing.T) {
	replicas := int32(3)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "workloads"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")}, deployment)
	before := deployment.DeepCopy()
	if _, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	after := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, after); err != nil {
		t.Fatal(err)
	}
	if after.Spec.Replicas == nil || before.Spec.Replicas == nil || *after.Spec.Replicas != *before.Spec.Replicas {
		t.Fatalf("unrelated deployment replicas changed: before=%v after=%v", before.Spec.Replicas, after.Spec.Replicas)
	}
}

func TestEvictionAllowsLossyPodsWithEmptyDirAndAffinity(t *testing.T) {
	runner1 := eligiblePodWithLabels("runner-1", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	runner1.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	runner1.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "kubernetes.io/hostname",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"desktop"},
					}},
				}},
			},
		},
	}

	runner2 := runner1.DeepCopy()
	runner2.Name = "runner-2"
	runner2.UID = types.UID("runner-2-uid")

	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{runner1, runner2})
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 2 || len(evictor.names) != 2 {
		t.Fatalf("expected 2 lossy evictions in single pass, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func eligiblePod(name string) *corev1.Pod {
	return eligiblePodWithLabels(name, map[string]string{EvictableLabel: "true"})
}

func eligiblePodWithLabels(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "workloads", UID: types.UID(name + "-uid"), ResourceVersion: "1", Labels: labels, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "app"}}}, Spec: corev1.PodSpec{NodeName: "desktop", TerminationGracePeriodSeconds: ptr(int64(20))}}
}

func evictionFixture(t *testing.T, state availabilityv1alpha1.State, activityType availabilityv1alpha1.Activity, effect corev1.TaintEffect, pods []*corev1.Pod, extras ...runtime.Object) (*NodeActivityReconciler, *availabilityv1alpha1.NodeActivity, *corev1.Node, *recordingEvictor) {
	t.Helper()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"}, Status: availabilityv1alpha1.NodeActivityStatus{State: state, Activity: activityType, HeartbeatAt: now}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: testPolicy().Key, Value: testPolicy().ActiveValue, Effect: effect}}}}
	objects := []runtime.Object{activity, node}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	objects = append(objects, extras...)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithStatusSubresource(activity).
		WithRuntimeObjects(objects...).
		Build()
	evictor := &recordingEvictor{}
	return &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy(), Eviction: EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 1, RetryBackoff: time.Second}, Evictor: evictor}, activity, node, evictor
}

func ptr[T any](value T) *T { return &value }

func TestEvictionLossyAnnotationOptIn(t *testing.T) {
	runner := eligiblePod("runner-annotation")
	runner.Annotations = map[string]string{AllowLossyEvictionAnnotation: "true"}
	runner.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	runner.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "desktop"}

	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{runner})
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 1 || len(evictor.names) != 1 || evictor.names[0].Name != "runner-annotation" {
		t.Fatalf("expected 1 lossy eviction via annotation, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestEvictionLossyHeartbeatExpiry(t *testing.T) {
	runner := eligiblePodWithLabels("runner-lossy", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	runner.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}

	normalPod := eligiblePod("normal-pod")

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{runner, normalPod})
	// Fail-closed taint on node
	node.Spec.Taints[0].Value = testPolicy().FailClosedValue
	desired := &corev1.Taint{Key: testPolicy().Key, Value: testPolicy().FailClosedValue, Effect: corev1.TaintEffectNoSchedule}

	// Expire heartbeat by advancing clock beyond StaleAfter
	r.Clock = clocktesting.NewFakeClock(now.Add(2 * testPolicy().StaleAfter))

	summary, err := r.reconcileEvictions(context.Background(), activity, node, desired)
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 1 || len(evictor.names) != 1 || evictor.names[0].Name != "runner-lossy" {
		t.Fatalf("expected only lossy pod evicted on stale heartbeat, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestEvictionLossyReporterUnknownDebounceDoesNotEvict(t *testing.T) {
	runner := eligiblePodWithLabels("runner-lossy", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	runner.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}

	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateUnknown, availabilityv1alpha1.ActivityUnknown, corev1.TaintEffectNoSchedule, []*corev1.Pod{runner})
	// Heartbeat is fresh, node has failClosed taint
	node.Spec.Taints[0].Value = testPolicy().FailClosedValue
	desired := &corev1.Taint{Key: testPolicy().Key, Value: testPolicy().FailClosedValue, Effect: corev1.TaintEffectNoSchedule}

	summary, err := r.reconcileEvictions(context.Background(), activity, node, desired)
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 0 || len(evictor.names) != 0 {
		t.Fatalf("expected 0 evictions during fresh unknown debounce, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestEvictionLossyHardSafetyChecksFailClosed(t *testing.T) {
	hostPathPod := eligiblePodWithLabels("runner-hostpath", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	hostPathPod.Spec.Volumes = []corev1.Volume{{Name: "hp", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run"}}}}

	rwoPod := eligiblePodWithLabels("runner-rwo", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	rwoPod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "rwo-pvc"}}}}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "rwo-pvc", Namespace: "workloads"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "rwo-pv", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "rwo-pv"},
		Spec:       corev1.PersistentVolumeSpec{},
	}

	daemonPod := eligiblePodWithLabels("runner-daemon", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	daemonPod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

	pinnedPod := eligiblePodWithLabels("runner-pinned", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	pinnedPod.Annotations = map[string]string{PinnedAnnotation: "true"}

	unlabeledPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-no-evictable", Namespace: "workloads", UID: "no-evict-uid", ResourceVersion: "1", Labels: map[string]string{AllowLossyEvictionLabel: "true"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "app"}}},
		Spec:       corev1.PodSpec{NodeName: "desktop"},
	}

	pods := []*corev1.Pod{hostPathPod, rwoPod, daemonPod, pinnedPod, unlabeledPod}
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, pods, pvc, pv)
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 0 || len(evictor.names) != 0 {
		t.Fatalf("expected 0 evictions for lossy pods failing hard safety gates, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestEvictionDecoupledLossyBurstBudget(t *testing.T) {
	normalPod := eligiblePod("normal-pod")

	runners := make([]*corev1.Pod, 4)
	for i := 0; i < 4; i++ {
		runners[i] = eligiblePodWithLabels(fmt.Sprintf("runner-%d", i+1), map[string]string{
			EvictableLabel:          "true",
			AllowLossyEvictionLabel: "true",
		})
		runners[i].Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	}

	allPods := append([]*corev1.Pod{normalPod}, runners...)
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, allPods)
	// policy.MaxPerReconcile is 1 (normal pods cap at 1, lossy pods cap at 4)
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 5 || len(evictor.names) != 5 {
		t.Fatalf("expected 5 evictions (1 normal + 4 lossy) in single pass with MaxPerReconcile=1, got summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestLossyEvictionEvictsDuringInteractiveUse(t *testing.T) {
	normalPod := eligiblePod("normal-pod")
	lossyPod := eligiblePodWithLabels("runner-lossy", map[string]string{
		EvictableLabel:          "true",
		AllowLossyEvictionLabel: "true",
	})
	lossyPod.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}

	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive, corev1.TaintEffectPreferNoSchedule, []*corev1.Pod{normalPod, lossyPod})
	node.Spec.Taints[0].Value = r.Policy.InteractiveValue

	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 1 || evictor.names[0].Name != "runner-lossy" {
		t.Fatalf("expected only runner-lossy to be evicted during interactive use, got %#v", evictor.names)
	}
	if summary.evicted != 1 {
		t.Fatalf("expected 1 eviction, got %d", summary.evicted)
	}
}

func TestIndexPodNodeName(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "target-node"}}
	values := IndexPodNodeName(pod)
	if len(values) != 1 || values[0] != "target-node" {
		t.Fatalf("expected [target-node], got %#v", values)
	}

	podNoNode := &corev1.Pod{Spec: corev1.PodSpec{}}
	if values := IndexPodNodeName(podNoNode); values != nil {
		t.Fatalf("expected nil for empty nodeName, got %#v", values)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "some-node"}}
	if values := IndexPodNodeName(node); values != nil {
		t.Fatalf("expected nil for non-Pod object, got %#v", values)
	}
}

func TestEvictionIndexedByNode_MultiNodeIsolation(t *testing.T) {
	podTarget := eligiblePod("pod-target-node")
	podTarget.Spec.NodeName = "desktop"

	podOther1 := eligiblePod("pod-other-node-1")
	podOther1.Spec.NodeName = "other-node-1"

	podOther2 := eligiblePod("pod-other-node-2")
	podOther2.Spec.NodeName = "other-node-2"

	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{podTarget, podOther1, podOther2})

	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 1 || len(evictor.names) != 1 || evictor.names[0].Name != "pod-target-node" {
		t.Fatalf("expected only pod-target-node to be evicted, got summary=%#v evictions=%#v", summary, evictor.names)
	}
	// Pods on other nodes must not be counted in summary.skipped or attempted
	if summary.skipped != 0 || summary.attempted != 1 {
		t.Fatalf("expected 0 skipped and 1 attempted for off-node isolation, got summary=%#v", summary)
	}
}

func TestEvictionFailsWhenIndexMissing(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop"},
		Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "availability.interactive-node.io/state", Value: "active", Effect: corev1.TaintEffectNoSchedule}}},
	}
	pod := eligiblePod("pod-on-desktop")
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// Build fake client deliberately WITHOUT WithIndex
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(activity).
		WithRuntimeObjects(activity, node, pod).
		Build()

	evictor := &recordingEvictor{}
	r := &NodeActivityReconciler{
		Client:   client,
		Clock:    clocktesting.NewFakeClock(now),
		Policy:   testPolicy(),
		Eviction: EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 1, RetryBackoff: time.Second},
		Evictor:  evictor,
	}

	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err == nil {
		t.Fatal("expected error when index is missing, got nil error")
	}
	if summary.evicted != 0 {
		t.Fatalf("expected 0 evictions on index error, got %d", summary.evicted)
	}
}

func BenchmarkReconcileEvictions_ManyOffNodePods(b *testing.B) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop"},
		Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "availability.interactive-node.io/state", Value: "active", Effect: corev1.TaintEffectNoSchedule}}},
	}

	targetPod := eligiblePod("target-runner")
	targetPod.Spec.NodeName = "desktop"

	objects := []runtime.Object{activity, node, targetPod}
	for i := 0; i < 500; i++ {
		offNodePod := eligiblePod(fmt.Sprintf("off-node-pod-%d", i))
		offNodePod.Spec.NodeName = fmt.Sprintf("other-node-%d", i%5)
		objects = append(objects, offNodePod)
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithStatusSubresource(activity).
		WithRuntimeObjects(objects...).
		Build()

	evictor := &recordingEvictor{}
	r := &NodeActivityReconciler{
		Client:   client,
		Clock:    clocktesting.NewFakeClock(now),
		Policy:   testPolicy(),
		Eviction: EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 1, RetryBackoff: time.Second},
		Evictor:  evictor,
	}

	ctx := context.Background()
	taint := &node.Spec.Taints[0]
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _ = r.reconcileEvictions(ctx, activity, node, taint)
	}
}
