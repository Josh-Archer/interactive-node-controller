package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/recovery"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	recoveryLeaderElectionID = "inc-recovery.availability.interactive-node.io"
	recoveryPauseImage       = "registry.k8s.io/pause:3.10"
	idleConditionMessage     = "host is idle; managed taint removed"
	interactiveMessage       = "host is interactive"
	gameMessage              = "host is running an active game"
	scheduledGameMessage     = "scheduled game window"
	staleConditionMessage    = "host heartbeat is missing or stale; fail-closed taint applied"
)

// TestDesktopRecoveryKind measures desktop recovery phases on a disposable
// Kind cluster. It refuses any kubeconfig whose current context is not kind-*.
// Eviction acceptance does not satisfy the resource-release sample.
func TestDesktopRecoveryKind(t *testing.T) {
	if os.Getenv("RECOVERY_KIND") != "true" {
		t.Skip("set RECOVERY_KIND=true and KUBECONFIG to a disposable kind cluster; use scripts/measure-desktop-recovery.sh")
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG is required and must point at a disposable kind cluster")
	}
	raw, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw.CurrentContext, "kind-") {
		t.Fatalf("refusing non-Kind kubeconfig context %q", raw.CurrentContext)
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctrl.SetLogger(logr.Discard())
	h := newRecoveryHarness(t, config, raw.CurrentContext)
	h.installCRD()
	h.createNamespace()
	h.chooseNode()
	h.rejectForeignEnrollment()
	h.leaderA = h.startLeader("recovery-a", true)
	h.createEnrollment()
	h.reporter.start()
	h.reporter.set(availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	h.waitIdleOpen()
	h.rememberTaints()
	h.addUnrelatedTaint()
	h.report.ProtectedNamespaces = h.snapshotNamespaces()
	h.createBaseWorkloads()
	h.scenarioStatus(recovery.ScenarioIdleToInteractive, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive, interactiveGate(), []string{recovery.WorkloadLossy})
	h.scenarioReturnToIdle()
	h.scenarioStatus(recovery.ScenarioIdleToGame, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, gameGate(), []string{recovery.WorkloadLossy, recovery.WorkloadStandard})
	h.restoreIdle()
	h.scenarioLead()
	h.clearSchedule()
	h.restoreIdle()
	h.scenarioReporterFailure()
	h.reporter.set(availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	h.restoreIdle()
	h.scenarioLeaderChange()
	h.restoreIdle()
	h.scenarioPDB()
	h.report.ProtectedNamespacesAfter = h.snapshotNamespaces()
	h.report.UnrelatedTaintPreserved = h.taintsPreserved()
	problems := recovery.Evaluate(h.report)
	h.writeReport()
	if len(problems) != 0 {
		t.Fatalf("recovery report rejected:\n%s", strings.Join(problems, "\n"))
	}
	t.Logf("desktop recovery report: %s", h.reportPath())
}

type nodeGate struct {
	message         string
	reason          string
	primaryValue    string
	primaryEffect   corev1.TaintEffect
	wantPrimary     bool
	wantBurst       bool
	wantSchedulable bool
}

func interactiveGate() nodeGate {
	return nodeGate{message: interactiveMessage, primaryValue: "interactive", primaryEffect: corev1.TaintEffectPreferNoSchedule, wantPrimary: true, wantBurst: true}
}

func gameGate() nodeGate {
	return nodeGate{message: gameMessage, primaryValue: "active", primaryEffect: corev1.TaintEffectNoSchedule, wantPrimary: true, wantBurst: true}
}

func staleGate() nodeGate {
	return nodeGate{message: staleConditionMessage, primaryValue: "unavailable", primaryEffect: corev1.TaintEffectNoSchedule, wantPrimary: true, wantBurst: true}
}

func scheduledGate() nodeGate {
	return nodeGate{message: scheduledGameMessage, reason: "ScheduledWindow", primaryValue: "active", primaryEffect: corev1.TaintEffectNoSchedule, wantPrimary: true, wantBurst: true}
}

func idleGate() nodeGate {
	return nodeGate{message: idleConditionMessage, wantSchedulable: true}
}

type runningLeader struct {
	cancel  context.CancelFunc
	elected <-chan struct{}
}

type syntheticReporter struct {
	h        *recoveryHarness
	mu       sync.Mutex
	state    availabilityv1alpha1.State
	activity availabilityv1alpha1.Activity
	stopped  bool
	stop     chan struct{}
	once     sync.Once
}

type recoveryHarness struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	config      *rest.Config
	direct      client.Client
	contextName string
	ns          string
	nodeName    string
	policy      TaintPolicy
	eviction    EvictionPolicy
	leaderA     runningLeader
	leaderB     *runningLeader
	reporter    *syntheticReporter
	report      recovery.Report
	current     *recovery.ScenarioResult
	preserved   []corev1.Taint
	stableUID   map[string]string
	generation  int64
	finished    bool
	accepted    *acceptanceRecorder
}

// acceptanceRecorder wraps the controller's Eviction client and records the
// moment the API server accepts each Eviction, keyed by the precondition UID.
// It is the controller's own acceptance signal, so a Pod deleted faster than
// any poll interval is still measured.
type acceptanceRecorder struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newAcceptanceRecorder() *acceptanceRecorder {
	return &acceptanceRecorder{at: map[string]time.Time{}}
}

type acceptanceEvictor struct {
	next     EvictionClient
	recorder *acceptanceRecorder
}

func (e acceptanceEvictor) Evict(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
	err := e.next.Evict(ctx, namespace, name, options)
	if err == nil && options.Preconditions != nil && options.Preconditions.UID != nil {
		e.recorder.record(string(*options.Preconditions.UID), time.Now())
	}
	return err
}

func (r *acceptanceRecorder) record(uid string, when time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.at[uid]; !seen {
		r.at[uid] = when
	}
}

func (r *acceptanceRecorder) lookup(uid string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	when, ok := r.at[uid]
	return when, ok
}

func newRecoveryHarness(t *testing.T, config *rest.Config, contextName string) *recoveryHarness {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	for _, add := range []func(*k8sruntime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		policyv1.AddToScheme,
		coordinationv1.AddToScheme,
		apiextensionsv1.AddToScheme,
		availabilityv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	policy := TaintPolicy{
		Key: "availability.interactive-node.io/state", InteractiveValue: "interactive", ActiveValue: "active",
		FailClosedValue: "unavailable", BurstTaintKey: "availability.interactive-node.io/burst",
		StaleAfter: recovery.DefaultBudget().HarnessStaleAfter.Duration(), FailClosed: true,
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	eviction := EvictionPolicy{
		Enabled: true, Audit: false, MaxPerReconcile: 4, RetryBackoff: 2 * time.Second, EvictableLabel: EvictableLabel,
		ProtectedNamespaces: map[string]struct{}{"kube-system": {}, "kube-public": {}, "kube-node-lease": {}},
	}
	if err := eviction.Validate(); err != nil {
		t.Fatal(err)
	}
	h := &recoveryHarness{
		t: t, ctx: ctx, cancel: cancel, config: config, direct: direct, contextName: contextName,
		ns: "inc62-" + strconv.FormatInt(time.Now().UnixNano(), 36), policy: policy, eviction: eviction,
		stableUID: map[string]string{},
		accepted:  newAcceptanceRecorder(),
		reporter:  &syntheticReporter{stop: make(chan struct{})},
		report: recovery.Report{
			SchemaVersion: recovery.SchemaVersion, Environment: recovery.EnvKind, DetectionScope: recovery.DetectionScopeController,
			Budget: recovery.DefaultBudget(), PerformedAt: time.Now().UTC(), ClusterContext: contextName, Controller: "in-process",
			GPUEvidence:     "kind has no GPU; nvidia.com/gpu was not requested and no host sample was taken",
			DesktopEvidence: "kind has no desktop session", WorkloadCatalog: recovery.Catalog(),
			LiveRunRemaining: recovery.KindLiveRunRemaining(),
		},
	}
	h.reporter.h = h
	t.Cleanup(h.finish)
	return h
}

func (h *recoveryHarness) installCRD() {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(h.t), "config/crd/bases/availability.interactive-node.io_nodeactivities.yaml"))
	if err != nil {
		h.t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(crd); err != nil {
		h.t.Fatal(err)
	}
	crd.ResourceVersion, crd.UID = "", ""
	crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
	existing := &apiextensionsv1.CustomResourceDefinition{}
	err = h.direct.Get(h.ctx, types.NamespacedName{Name: crd.Name}, existing)
	if apierrors.IsNotFound(err) {
		if err := h.direct.Create(h.ctx, crd); err != nil {
			h.t.Fatal(err)
		}
	} else if err != nil {
		h.t.Fatal(err)
	}
	h.poll(30*time.Second, "CRD Established", func(ctx context.Context) (bool, error) {
		current := &apiextensionsv1.CustomResourceDefinition{}
		if err := h.direct.Get(ctx, types.NamespacedName{Name: crd.Name}, current); err != nil {
			return false, err
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

func (h *recoveryHarness) createNamespace() {
	h.t.Helper()
	if err := h.direct.Create(h.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: h.ns}}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *recoveryHarness) chooseNode() {
	h.t.Helper()
	list := &corev1.NodeList{}
	if err := h.direct.List(h.ctx, list); err != nil {
		h.t.Fatal(err)
	}
	for i := range list.Items {
		node := &list.Items[i]
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				h.nodeName = node.Name
				h.report.NodeName = node.Name
				return
			}
		}
	}
	h.t.Fatal("Kind cluster has no Ready node")
}

func (h *recoveryHarness) rejectForeignEnrollment() {
	h.t.Helper()
	list := &availabilityv1alpha1.NodeActivityList{}
	if err := h.direct.List(h.ctx, list); err != nil {
		h.t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.Spec.NodeName == h.nodeName {
			h.t.Fatalf("node %s is already enrolled by %s/%s; the harness refuses to share a node", h.nodeName, item.Namespace, item.Name)
		}
	}
}

func (h *recoveryHarness) startLeader(name string, waitElected bool) runningLeader {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	lease, renew, retryPeriod := 5*time.Second, 3*time.Second, time.Second
	manager, err := ctrl.NewManager(h.config, ctrl.Options{
		Scheme: h.direct.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		LeaderElection: true, LeaderElectionID: recoveryLeaderElectionID, LeaderElectionNamespace: h.ns,
		LeaderElectionReleaseOnCancel: true, LeaseDuration: &lease, RenewDeadline: &renew, RetryPeriod: &retryPeriod,
	})
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(h.config)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	reconciler := &NodeActivityReconciler{
		Client: manager.GetClient(), Policy: h.policy, Eviction: h.eviction,
		Evictor: acceptanceEvictor{next: KubernetesEvictionClient{Client: clientset}, recorder: h.accepted}, controllerName: name,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		cancel()
		h.t.Fatal(err)
	}
	go func() {
		err := manager.Start(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "leader election lost") {
			h.t.Errorf("manager %s: %v", name, err)
		}
	}()
	leader := runningLeader{cancel: cancel, elected: manager.Elected()}
	if waitElected {
		h.waitElected(leader, 30*time.Second)
	}
	return leader
}

func (h *recoveryHarness) waitElected(leader runningLeader, timeout time.Duration) {
	h.t.Helper()
	select {
	case <-leader.elected:
	case <-time.After(timeout):
		h.failf("controller did not become leader within %s", timeout)
	case <-h.ctx.Done():
		h.failf("context ended while waiting for leader: %v", h.ctx.Err())
	}
}

func (h *recoveryHarness) createEnrollment() {
	h.t.Helper()
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: h.ns},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: h.nodeName},
	}
	if err := h.direct.Create(h.ctx, activity); err != nil {
		h.t.Fatal(err)
	}
}

func (r *syntheticReporter) start() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.stopped || r.state == "" {
					r.mu.Unlock()
					continue
				}
				state, activity := r.state, r.activity
				err := r.h.patchStatus(state, activity)
				r.mu.Unlock()
				if err != nil && r.h.ctx.Err() == nil {
					r.h.t.Logf("synthetic heartbeat: %v", err)
				}
			}
		}
	}()
}

func (r *syntheticReporter) set(state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = false
	r.state, r.activity = state, activity
	if err := r.h.patchStatus(state, activity); err != nil {
		r.h.failf("synthetic reporter patch: %v", err)
	}
	return time.Now()
}

func (r *syntheticReporter) halt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	return time.Now()
}

func (r *syntheticReporter) shutdown() {
	r.once.Do(func() { close(r.stop) })
	r.halt()
}

func (h *recoveryHarness) patchStatus(state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity) error {
	h.generation++
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]availabilityv1alpha1.NodeActivityStatus{"status": {
		State: state, Activity: activity, Reason: "synthetic-reporter", ObservedAt: now, HeartbeatAt: now,
		Generation: h.generation, FailClosed: true,
	}})
	if err != nil {
		return err
	}
	object := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: h.ns}}
	return h.direct.Status().Patch(h.ctx, object, client.RawPatch(types.MergePatchType, body))
}

func (h *recoveryHarness) createBaseWorkloads() {
	h.t.Helper()
	for _, name := range []string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned} {
		h.ensureDeployment(name)
	}
	for _, name := range []string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned} {
		view := h.waitRunning(name)
		if name == recovery.WorkloadProtected || name == recovery.WorkloadPinned {
			h.stableUID[name] = view.uid
		}
	}
}

func (h *recoveryHarness) ensureDeployment(name string) {
	h.t.Helper()
	profile, ok := recovery.Lookup(name)
	if !ok {
		h.t.Fatalf("unknown workload %s", name)
	}
	labels := map[string]string{"app.kubernetes.io/name": profile.Name, "app.kubernetes.io/part-of": "desktop-recovery-canary"}
	if profile.OptedIn {
		labels[EvictableLabel] = "true"
	}
	if profile.Lossy {
		labels[AllowLossyEvictionLabel] = "true"
	}
	var annotations map[string]string
	if profile.Pinned {
		annotations = map[string]string{PinnedAnnotation: "true"}
	}
	grace := int64(1)
	if profile.Disposable && profile.OptedIn {
		grace = 3
	}
	podSpec := corev1.PodSpec{
		TerminationGracePeriodSeconds: &grace, Tolerations: recoveryTolerations(),
		Containers: []corev1.Container{{
			Name: "pause", Image: recoveryPauseImage, ImagePullPolicy: corev1.PullIfNotPresent,
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(profile.CPU), corev1.ResourceMemory: resource.MustParse(profile.Memory),
			}},
		}},
	}
	if profile.NodeSelector {
		podSpec.NodeSelector = map[string]string{SchedulableNodeLabel: "true"}
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: profile.Name, Namespace: h.ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": profile.Name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: podSpec},
		},
	}
	if err := h.direct.Create(h.ctx, deployment); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatal(err)
	}
}

func recoveryTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists},
		{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists},
		{Key: recovery.UnrelatedTaintKey, Operator: corev1.TolerationOpExists},
	}
}

type podView struct {
	uid   string
	phase string
	holds bool
}

func (h *recoveryHarness) waitRunning(name string) podView {
	h.t.Helper()
	var view podView
	h.poll(90*time.Second, name+" Running", func(ctx context.Context) (bool, error) {
		current, ok := h.runningPod(ctx, name)
		if !ok {
			return false, nil
		}
		view = current
		return true, nil
	})
	h.assertCatalogRequests(name)
	return view
}

func (h *recoveryHarness) runningPod(ctx context.Context, name string) (podView, bool) {
	for _, pod := range h.pods(ctx, name) {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil && pod.Spec.NodeName == h.nodeName {
			return podView{uid: string(pod.UID), phase: string(pod.Status.Phase), holds: true}, true
		}
	}
	return podView{}, false
}

func (h *recoveryHarness) pods(ctx context.Context, name string) []corev1.Pod {
	list := &corev1.PodList{}
	if err := h.direct.List(ctx, list, client.InNamespace(h.ns), client.MatchingLabels{"app.kubernetes.io/name": name}); err != nil {
		return nil
	}
	return list.Items
}

func (h *recoveryHarness) holds(ctx context.Context, name string) bool {
	for _, pod := range h.pods(ctx, name) {
		if pod.Spec.NodeName == h.nodeName && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return true
		}
	}
	return false
}

func (h *recoveryHarness) assertCatalogRequests(name string) {
	h.t.Helper()
	profile, _ := recovery.Lookup(name)
	for _, pod := range h.pods(h.ctx, name) {
		if pod.DeletionTimestamp != nil {
			continue
		}
		var cpu, memory int64
		for _, container := range pod.Spec.Containers {
			if container.Resources.Requests != nil {
				cpu += container.Resources.Requests.Cpu().MilliValue()
				memory += container.Resources.Requests.Memory().Value()
			}
			if quantity, ok := container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok && !quantity.IsZero() {
				h.failf("%s requests a GPU", name)
			}
		}
		if cpu != profile.CPUMillis || memory != profile.MemoryBytes {
			h.failf("%s requests cpu=%dm memory=%d, catalog is %dm/%d", name, cpu, memory, profile.CPUMillis, profile.MemoryBytes)
		}
		opted := pod.Labels[EvictableLabel] == "true"
		lossy := strings.EqualFold(pod.Labels[AllowLossyEvictionLabel], "true")
		pinned := pod.Annotations[PinnedAnnotation] == "true"
		if opted != profile.OptedIn || lossy != profile.Lossy || pinned != profile.Pinned {
			h.failf("%s opt-in labels do not match the catalog", name)
		}
		return
	}
}

func (h *recoveryHarness) scenarioStatus(name string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity, gate nodeGate, evict []string) {
	h.t.Helper()
	h.begin(name)
	before := h.snapshotWorkloads([]string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned})
	trigger := h.reporter.set(state, activity)
	clock := h.collect(trigger, gate, evict, before)
	h.current.Samples = h.recoverySamples(name, recovery.TriggerSyntheticStatus, trigger, clock, true)
	h.current.Workloads = h.observations(statusWorkloads, before, evictSet(evict), "")
	h.endScenario()
}

func (h *recoveryHarness) scenarioReturnToIdle() {
	h.t.Helper()
	h.begin(recovery.ScenarioReturnToIdle)
	before := h.snapshotWorkloads([]string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned})
	trigger := h.reporter.set(availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	detected := h.waitGate(idleGate(), recovery.AllowedBudget(h.report.Budget, recovery.ScenarioReturnToIdle, recovery.PhaseDetection))
	reopened := h.waitProbe(true, recovery.AllowedBudget(h.report.Budget, recovery.ScenarioReturnToIdle, recovery.PhaseAdmissionReopen))
	h.waitRunning(recovery.WorkloadLossy)
	h.waitRunning(recovery.WorkloadStandard)
	h.current.Samples = []recovery.PhaseSample{
		h.sample(recovery.ScenarioReturnToIdle, recovery.PhaseDetection, recovery.TriggerSyntheticStatus, trigger, detected, recovery.ExpectedObserved, "condition="+idleConditionMessage, false, nil),
		h.sample(recovery.ScenarioReturnToIdle, recovery.PhaseAdmissionReopen, recovery.TriggerSyntheticStatus, trigger, reopened, recovery.ExpectedReopened, "schedulable label restored and probe Running", false, nil),
		h.inactive(recovery.ScenarioReturnToIdle, recovery.PhaseEviction),
		h.inactive(recovery.ScenarioReturnToIdle, recovery.PhaseRelease),
	}
	h.current.Workloads = h.observations(statusWorkloads, before, nil, "")
	h.endScenario()
}

func (h *recoveryHarness) scenarioLead() {
	h.t.Helper()
	h.begin(recovery.ScenarioScheduledLead)
	window, err := recovery.NextLeadWindow(time.Now())
	if err != nil {
		h.failf("lead window: %v", err)
	}
	h.setSchedule(window)
	h.assertIdleUntil(window.Boundary.Add(-time.Second))
	before := h.snapshotWorkloads(statusWorkloads)
	time.Sleep(time.Until(window.Boundary))
	clock := h.collect(window.Boundary, scheduledGate(), []string{recovery.WorkloadLossy}, before)
	h.current.Samples = h.recoverySamples(recovery.ScenarioScheduledLead, recovery.TriggerLeadBoundary, window.Boundary, clock, true)
	h.current.Workloads = h.observations(statusWorkloads, before, evictSet([]string{recovery.WorkloadLossy}), "")
	h.current.LeadTime = recovery.JSONDuration(window.Lead)
	h.current.WindowStart = &window.Start
	h.current.PreWindowReleaseProven = false
	h.current.Notes = append(h.current.Notes, "harness lead is shorter than the end-to-end budget; pre-window CPU/memory release is not claimed")
	h.endScenario()
}

func (h *recoveryHarness) scenarioReporterFailure() {
	h.t.Helper()
	h.begin(recovery.ScenarioReporterFailure)
	before := h.snapshotWorkloads([]string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned})
	trigger := h.reporter.halt()
	clock := h.collect(trigger, staleGate(), []string{recovery.WorkloadLossy}, before)
	h.current.Samples = h.recoverySamples(recovery.ScenarioReporterFailure, recovery.TriggerReporterStopped, trigger, clock, true)
	h.current.Workloads = h.observations(statusWorkloads, before, evictSet([]string{recovery.WorkloadLossy}), "")
	h.endScenario()
}

func (h *recoveryHarness) scenarioLeaderChange() {
	h.t.Helper()
	h.begin(recovery.ScenarioControllerRestart)
	follower := h.startLeader("recovery-b", false)
	h.leaderB = &follower
	time.Sleep(time.Second)
	stopped := time.Now()
	h.leaderA.cancel()
	h.waitElected(follower, recovery.DefaultBudget().LeaderRestore.Duration())
	elected := time.Now()
	drift := h.disturbIdleNode()
	restored := h.waitGate(idleGate(), recovery.DefaultBudget().LeaderRestore.Duration())
	before := h.snapshotWorkloads([]string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned})
	trigger := h.reporter.set(availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame)
	clock := h.collect(trigger, gameGate(), []string{recovery.WorkloadLossy, recovery.WorkloadStandard}, before)
	h.current.Samples = []recovery.PhaseSample{
		h.sample(recovery.ScenarioControllerRestart, recovery.PhaseLeaderElection, recovery.TriggerLeaderCancelled, stopped, elected, recovery.ExpectedObserved, "new leader elected holder="+h.leaseHolder(), false, nil),
		h.sample(recovery.ScenarioControllerRestart, recovery.PhaseLeaderRestore, recovery.TriggerDrift, drift, restored, recovery.ExpectedObserved, "idle protection restored after node drift", false, nil),
	}
	h.current.Samples = append(h.current.Samples, h.recoverySamples(recovery.ScenarioControllerRestart, recovery.TriggerSyntheticStatus, trigger, clock, true)...)
	h.current.Workloads = h.observations(statusWorkloads, before, evictSet([]string{recovery.WorkloadLossy, recovery.WorkloadStandard}), "")
	h.endScenario()
}

func (h *recoveryHarness) scenarioPDB() {
	h.t.Helper()
	h.begin(recovery.ScenarioPDB)
	h.ensureDeployment(recovery.WorkloadPDB)
	h.createPDB()
	h.waitRunning(recovery.WorkloadPDB)
	h.waitPDBBlocked()
	before := h.snapshotWorkloads([]string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned, recovery.WorkloadPDB})
	trigger := h.reporter.set(availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive)
	clock := h.collect(trigger, interactiveGate(), []string{recovery.WorkloadLossy}, before)
	h.assertStillBlocked(recovery.WorkloadPDB, before[recovery.WorkloadPDB].uid)
	blockedAt := time.Now()
	blocked := h.observations([]string{recovery.WorkloadPDB}, before, nil, "blocked")
	h.current.Blocked = []recovery.BlockedCase{{
		Workload: recovery.WorkloadPDB, Reason: "pdb",
		Evidence: recovery.EvidencePDB + " " + recovery.EvidenceStillBound + " uid=" + before[recovery.WorkloadPDB].uid,
	}}
	h.current.Samples = h.recoverySamples(recovery.ScenarioPDB, recovery.TriggerSyntheticStatus, trigger, clock, false)
	h.current.Samples = append(h.current.Samples,
		h.sample(recovery.ScenarioPDB, recovery.PhaseEviction, recovery.TriggerSyntheticStatus, trigger, blockedAt, recovery.ExpectedBlocked, recovery.EvidenceTimestampAbsent+" "+recovery.EvidencePDB, false, nil),
		h.sample(recovery.ScenarioPDB, recovery.PhaseRelease, recovery.TriggerSyntheticStatus, trigger, blockedAt, recovery.ExpectedBlocked, recovery.EvidenceStillBound+" "+recovery.WorkloadPDB, false, nil),
	)
	removed := h.deletePDB()
	accept, release := h.waitEvicted(recovery.WorkloadPDB, before[recovery.WorkloadPDB].uid, removed)
	gap := release.Sub(accept).Milliseconds()
	if gap < 1000 {
		h.failf("pdb clearance acceptance gap = %dms", gap)
	}
	h.current.Samples = append(h.current.Samples,
		h.sample(recovery.ScenarioPDB, recovery.PhaseEviction, recovery.TriggerPDBDeleted, removed, accept, recovery.ExpectedObserved, recovery.EvidenceEvictionAccepted, false, nil),
		h.sample(recovery.ScenarioPDB, recovery.PhaseRelease, recovery.TriggerPDBDeleted, removed, release, recovery.ExpectedObserved, recovery.EvidencePodAbsent+" "+recovery.EvidenceRequestsDropped+" "+recovery.EvidenceAfterPDBRemoved, true, &gap),
	)
	h.current.Workloads = append(h.observations(statusWorkloads, before, evictSet([]string{recovery.WorkloadLossy}), ""), blocked...)
	h.current.Workloads = append(h.current.Workloads, h.observations([]string{recovery.WorkloadPDB}, before, evictSet([]string{recovery.WorkloadPDB}), "cleared")...)
	h.endScenario()
}

type phaseClock struct {
	detection  time.Time
	admission  time.Time
	acceptance time.Time
	release    time.Time
	gap        int64
}

func (h *recoveryHarness) collect(trigger time.Time, gate nodeGate, evict []string, before map[string]podView) phaseClock {
	h.t.Helper()
	timeout := recovery.DefaultBudget().EndToEnd.Duration()
	deadline := time.Now().Add(timeout)
	var clock phaseClock
	var probeReady bool
	accepted := map[string]time.Time{}
	released := map[string]time.Time{}
	for time.Now().Before(deadline) && (clock.detection.IsZero() || clock.admission.IsZero() || len(accepted) < len(evict) || len(released) < len(evict)) {
		if clock.detection.IsZero() && h.conditionMatches(gate) {
			clock.detection = time.Now()
		}
		if !probeReady && h.nodeMatches(gate) {
			probeReady = true
			h.createProbe()
		}
		if probeReady && clock.admission.IsZero() && h.probePending() {
			clock.admission = time.Now()
		}
		for _, name := range evict {
			state, ok := before[name]
			if !ok || state.uid == "" {
				h.failf("%s had no running pod at the trigger", name)
			}
			if _, seen := accepted[name]; !seen {
				if when, ok := h.acceptedAt(state.uid); ok {
					accepted[name] = when
				}
			}
			if _, seen := released[name]; !seen && !h.holds(h.ctx, name) && !h.podExists(name, state.uid) {
				released[name] = time.Now()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clock.detection.IsZero() || clock.admission.IsZero() {
		h.failf("detection=%s admission=%s node=%s condition=%s", clock.detection, clock.admission, h.nodeSummary(), h.conditionSummary())
	}
	gap := 24 * time.Hour
	for _, name := range evict {
		accept, release := accepted[name], released[name]
		if accept.IsZero() || release.IsZero() {
			h.failf("%s acceptance=%s release=%s", name, accept, release)
		}
		if release.Sub(accept) < gap {
			gap = release.Sub(accept)
		}
		if accept.After(clock.acceptance) {
			clock.acceptance = accept
		}
		if release.After(clock.release) {
			clock.release = release
		}
	}
	if len(evict) > 0 {
		clock.gap = gap.Milliseconds()
		if clock.gap < 1000 {
			h.failf("acceptance-to-release gap = %dms; eviction acceptance is not resource release", clock.gap)
		}
	}
	return clock
}

func (h *recoveryHarness) recoverySamples(scenario, trigger string, triggerAt time.Time, clock phaseClock, includeRelease bool) []recovery.PhaseSample {
	samples := []recovery.PhaseSample{
		h.sample(scenario, recovery.PhaseDetection, trigger, triggerAt, clock.detection, recovery.ExpectedObserved, "condition="+h.conditionSummary(), false, nil),
		h.sample(scenario, recovery.PhaseAdmission, trigger, triggerAt, clock.admission, recovery.ExpectedObserved, "taint and schedulable label changed; probe has no nodeName", false, nil),
		h.sample(scenario, recovery.PhaseEviction, trigger, triggerAt, clock.acceptance, recovery.ExpectedObserved, recovery.EvidenceEvictionAccepted, false, nil),
	}
	if includeRelease {
		gap := clock.gap
		samples = append(samples, h.sample(scenario, recovery.PhaseRelease, trigger, triggerAt, clock.release, recovery.ExpectedObserved, recovery.EvidencePodAbsent+" "+recovery.EvidenceRequestsDropped, true, &gap))
	}
	return samples
}

func (h *recoveryHarness) sample(scenario, phase, trigger string, triggerAt, observed time.Time, expected, evidence string, satisfies bool, gap *int64) recovery.PhaseSample {
	duration := observed.Sub(triggerAt).Milliseconds()
	if duration < 0 {
		h.failf("%s %s observed before trigger", scenario, phase)
	}
	allowed := recovery.AllowedBudget(h.report.Budget, scenario, phase)
	return recovery.PhaseSample{
		Phase: phase, Applicable: true, Expected: expected, Trigger: trigger, TriggerAt: triggerAt.UTC(), ObservedAt: timePtr(observed.UTC()),
		DurationMillis: &duration, Budget: recovery.JSONDuration(allowed), WithinBudget: time.Duration(duration)*time.Millisecond <= allowed,
		SatisfiesRecovery: satisfies, AcceptanceGapMillis: gap, Evidence: evidence,
	}
}

func (h *recoveryHarness) inactive(scenario, phase string) recovery.PhaseSample {
	return recovery.PhaseSample{
		Phase: phase, Expected: recovery.ExpectedAbsent, Trigger: recovery.TriggerSyntheticStatus,
		Budget: recovery.JSONDuration(recovery.AllowedBudget(h.report.Budget, scenario, phase)), WithinBudget: true,
		Evidence: recovery.EvidenceUIDUnchanged,
	}
}

func (h *recoveryHarness) snapshotWorkloads(names []string) map[string]podView {
	out := map[string]podView{}
	for _, name := range names {
		if view, ok := h.runningPod(h.ctx, name); ok {
			out[name] = view
		}
	}
	return out
}

var statusWorkloads = []string{recovery.WorkloadLossy, recovery.WorkloadStandard, recovery.WorkloadProtected, recovery.WorkloadPinned}

func (h *recoveryHarness) observations(names []string, before map[string]podView, evict map[string]bool, stage string) []recovery.WorkloadObservation {
	var out []recovery.WorkloadObservation
	for _, name := range names {
		if _, ok := before[name]; !ok && name == recovery.WorkloadPDB {
			continue
		}
		profile, _ := recovery.Lookup(name)
		prior := before[name]
		after, running := h.runningPod(h.ctx, name)
		evicted := evict[name]
		observation := recovery.WorkloadObservation{
			Name: profile.Name, Role: profile.Role, Disposable: profile.Disposable, OptedIn: profile.OptedIn,
			Lossy: profile.Lossy, Pinned: profile.Pinned, CPUMillis: profile.CPUMillis, MemoryBytes: profile.MemoryBytes,
			UIDBefore: prior.uid, UIDAfter: after.uid, PhaseAfter: after.phase, HoldsResourcesAfter: h.holds(h.ctx, name),
			EvictedDuringScenario: evicted, Stage: stage,
		}
		if evicted {
			observation.HoldsResourcesAfter = h.holds(h.ctx, name)
			if !running {
				observation.UIDAfter = ""
				observation.PhaseAfter = "Absent"
			}
		} else if prior.uid == "" && running {
			observation.UIDAfter = after.uid
			observation.PhaseAfter = after.phase
			observation.HoldsResourcesAfter = true
		}
		out = append(out, observation)
	}
	return out
}

func (h *recoveryHarness) begin(name string) {
	h.t.Helper()
	h.deleteProbes()
	h.assertStable()
	h.current = &recovery.ScenarioResult{Name: name}
}

func (h *recoveryHarness) endScenario() {
	h.t.Helper()
	h.deleteProbes()
	h.assertStable()
	h.assertProtectedNamespaces()
	h.finishScenario()
}

func (h *recoveryHarness) finishScenario() {
	if h.current == nil {
		return
	}
	h.report.Scenarios = append(h.report.Scenarios, *h.current)
	h.current = nil
}

func (h *recoveryHarness) restoreIdle() {
	h.t.Helper()
	h.reporter.set(availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	h.waitIdleOpen()
	h.waitRunning(recovery.WorkloadLossy)
	h.waitRunning(recovery.WorkloadStandard)
	h.assertStable()
	h.deleteProbes()
}

func (h *recoveryHarness) waitIdleOpen() {
	h.t.Helper()
	h.waitGate(idleGate(), 45*time.Second)
}

func (h *recoveryHarness) waitGate(gate nodeGate, timeout time.Duration) time.Time {
	h.t.Helper()
	var observed time.Time
	h.poll(timeout, "gate "+gate.message, func(ctx context.Context) (bool, error) {
		if h.conditionMatches(gate) && h.nodeMatches(gate) {
			observed = time.Now()
			return true, nil
		}
		return false, nil
	})
	return observed
}

func (h *recoveryHarness) conditionMatches(gate nodeGate) bool {
	reason, message := h.condition()
	if gate.reason != "" && reason != gate.reason {
		return false
	}
	return message == gate.message
}

func (h *recoveryHarness) nodeMatches(gate nodeGate) bool {
	node := h.node()
	if node == nil {
		return false
	}
	primary := hasTaint(node, h.policy.Key, gate.primaryValue, gate.primaryEffect)
	if gate.wantPrimary != primary {
		return false
	}
	if !gate.wantPrimary && h.ownedPrimary(node) {
		return false
	}
	burst := hasTaint(node, h.policy.BurstTaintKey, h.policy.FailClosedValue, corev1.TaintEffectNoSchedule)
	if gate.wantBurst != burst {
		return false
	}
	schedulable := node.Labels[SchedulableNodeLabel] == "true"
	return schedulable == gate.wantSchedulable
}

func (h *recoveryHarness) ownedPrimary(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == h.policy.Key {
			return true
		}
	}
	return false
}

func (h *recoveryHarness) condition() (string, string) {
	activity := &availabilityv1alpha1.NodeActivity{}
	if err := h.direct.Get(h.ctx, types.NamespacedName{Name: "desktop", Namespace: h.ns}, activity); err != nil {
		return "", ""
	}
	condition := meta.FindStatusCondition(activity.Status.Conditions, TaintAppliedCondition)
	if condition == nil {
		return "", ""
	}
	return condition.Reason, condition.Message
}

func (h *recoveryHarness) node() *corev1.Node {
	node := &corev1.Node{}
	if err := h.direct.Get(h.ctx, types.NamespacedName{Name: h.nodeName}, node); err != nil {
		return nil
	}
	return node
}

func (h *recoveryHarness) createProbe() {
	profile, _ := recovery.Lookup(recovery.WorkloadProbe)
	name := "probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h.probeCreate(name, profile)
}

func (h *recoveryHarness) probeCreate(name string, profile recovery.WorkloadProfile) {
	grace := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns, Labels: map[string]string{"app.kubernetes.io/name": recovery.WorkloadProbe, "desktop-recovery.example/probe": "true"}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: &grace, Tolerations: recoveryTolerations(),
			NodeSelector: map[string]string{SchedulableNodeLabel: "true"},
			Containers: []corev1.Container{{
				Name: "pause", Image: recoveryPauseImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse(profile.CPU), corev1.ResourceMemory: resource.MustParse(profile.Memory),
				}},
			}},
		},
	}
	if err := h.direct.Create(h.ctx, pod); err != nil {
		h.failf("create probe: %v", err)
	}
}

func (h *recoveryHarness) probePending() bool {
	for _, pod := range h.pods(h.ctx, recovery.WorkloadProbe) {
		if pod.Labels["desktop-recovery.example/probe"] == "true" && pod.Spec.NodeName == "" && pod.Status.Phase == corev1.PodPending {
			return true
		}
	}
	return false
}

func (h *recoveryHarness) waitProbe(running bool, timeout time.Duration) time.Time {
	h.t.Helper()
	h.createProbe()
	var observed time.Time
	h.poll(timeout, "admission probe", func(ctx context.Context) (bool, error) {
		for _, pod := range h.pods(ctx, recovery.WorkloadProbe) {
			if pod.Labels["desktop-recovery.example/probe"] != "true" {
				continue
			}
			if running && pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName == h.nodeName {
				observed = time.Now()
				return true, nil
			}
			if !running && pod.Spec.NodeName == "" && pod.Status.Phase == corev1.PodPending {
				observed = time.Now()
				return true, nil
			}
		}
		return false, nil
	})
	return observed
}

func (h *recoveryHarness) deleteProbes() {
	grace := int64(0)
	for _, pod := range h.pods(h.ctx, recovery.WorkloadProbe) {
		_ = h.direct.Delete(h.ctx, &pod, &client.DeleteOptions{GracePeriodSeconds: &grace})
	}
	_ = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		return len(h.pods(ctx, recovery.WorkloadProbe)) == 0, nil
	})
}

// acceptedAt reports when the controller's Eviction request for this Pod UID
// was accepted by the API server.
func (h *recoveryHarness) acceptedAt(uid string) (time.Time, bool) {
	return h.accepted.lookup(uid)
}

func (h *recoveryHarness) podExists(name, uid string) bool {
	for _, pod := range h.pods(h.ctx, name) {
		if string(pod.UID) == uid {
			return true
		}
	}
	return false
}

func (h *recoveryHarness) waitEvicted(name, uid string, trigger time.Time) (time.Time, time.Time) {
	h.t.Helper()
	var accept, release time.Time
	deadline := trigger.Add(recovery.DefaultBudget().ResourceRelease.Duration())
	for time.Now().Before(deadline) && (accept.IsZero() || release.IsZero()) {
		if accept.IsZero() {
			if when, ok := h.acceptedAt(uid); ok {
				accept = when
			}
		}
		if release.IsZero() && !h.holds(h.ctx, name) && !h.podExists(name, uid) {
			release = time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if accept.IsZero() || release.IsZero() {
		h.failf("%s did not release after %s (accept=%s release=%s)", name, trigger, accept, release)
	}
	return accept, release
}

func (h *recoveryHarness) createPDB() {
	h.t.Helper()
	minAvailable := intstr.FromInt(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: recovery.WorkloadPDB, Namespace: h.ns},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvailable, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": recovery.WorkloadPDB}}},
	}
	if err := h.direct.Create(h.ctx, pdb); err != nil {
		h.t.Fatal(err)
	}
}

func (h *recoveryHarness) waitPDBBlocked() {
	h.t.Helper()
	h.poll(30*time.Second, "PDB disruptionsAllowed=0", func(ctx context.Context) (bool, error) {
		pdb := &policyv1.PodDisruptionBudget{}
		if err := h.direct.Get(ctx, types.NamespacedName{Name: recovery.WorkloadPDB, Namespace: h.ns}, pdb); err != nil {
			return false, err
		}
		return pdb.Status.ObservedGeneration >= pdb.Generation && pdb.Status.DisruptionsAllowed == 0, nil
	})
}

func (h *recoveryHarness) assertStillBlocked(name, uid string) {
	h.t.Helper()
	time.Sleep(3 * time.Second)
	for _, pod := range h.pods(h.ctx, name) {
		if string(pod.UID) == uid {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
				h.failf("PDB workload %s was disrupted: phase=%s deletion=%v", name, pod.Status.Phase, pod.DeletionTimestamp)
			}
			return
		}
	}
	h.failf("PDB workload %s disappeared", name)
}

func (h *recoveryHarness) deletePDB() time.Time {
	h.t.Helper()
	if err := h.direct.Delete(h.ctx, &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: recovery.WorkloadPDB, Namespace: h.ns}}); err != nil {
		h.failf("delete pdb: %v", err)
	}
	return time.Now()
}

func (h *recoveryHarness) setSchedule(window recovery.LeadWindow) {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		activity := &availabilityv1alpha1.NodeActivity{}
		if err := h.direct.Get(h.ctx, types.NamespacedName{Name: "desktop", Namespace: h.ns}, activity); err != nil {
			return err
		}
		if window.Schedule == "" {
			activity.Spec.LeadTime = ""
			activity.Spec.Windows = nil
		} else {
			activity.Spec.LeadTime = window.Lead.String()
			activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{
				Schedule: window.Schedule, Duration: window.Duration, TimeZone: "UTC", State: window.State,
			}}
		}
		return h.direct.Update(h.ctx, activity)
	})
	if err != nil {
		h.failf("update schedule: %v", err)
	}
}

func (h *recoveryHarness) clearSchedule() {
	h.setSchedule(recovery.LeadWindow{})
}

func (h *recoveryHarness) assertIdleUntil(until time.Time) {
	h.t.Helper()
	for time.Now().Before(until) {
		if !h.conditionMatches(idleGate()) || !h.nodeMatches(idleGate()) {
			h.failf("schedule applied before the lead boundary: condition=%s node=%s", h.conditionSummary(), h.nodeSummary())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (h *recoveryHarness) disturbIdleNode() time.Time {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node := &corev1.Node{}
		if err := h.direct.Get(h.ctx, types.NamespacedName{Name: h.nodeName}, node); err != nil {
			return err
		}
		delete(node.Labels, SchedulableNodeLabel)
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: h.policy.Key, Value: h.policy.InteractiveValue, Effect: corev1.TaintEffectNoSchedule})
		return h.direct.Update(h.ctx, node)
	})
	if err != nil {
		h.failf("disturb node: %v", err)
	}
	return time.Now()
}

func (h *recoveryHarness) leaseHolder() string {
	lease := &coordinationv1.Lease{}
	if err := h.direct.Get(h.ctx, types.NamespacedName{Name: recoveryLeaderElectionID, Namespace: h.ns}, lease); err != nil || lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func (h *recoveryHarness) rememberTaints() {
	node := h.node()
	if node == nil {
		h.t.Fatal("node disappeared before taint snapshot")
	}
	h.preserved = append([]corev1.Taint(nil), node.Spec.Taints...)
}

func (h *recoveryHarness) addUnrelatedTaint() {
	h.t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node := &corev1.Node{}
		if err := h.direct.Get(h.ctx, types.NamespacedName{Name: h.nodeName}, node); err != nil {
			return err
		}
		for _, taint := range node.Spec.Taints {
			if taint.Key == recovery.UnrelatedTaintKey {
				return nil
			}
		}
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: recovery.UnrelatedTaintKey, Value: "keep", Effect: corev1.TaintEffectNoSchedule})
		return h.direct.Update(h.ctx, node)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.poll(15*time.Second, "unrelated taint preserved while idle", func(ctx context.Context) (bool, error) {
		return h.taintsPreserved() && h.nodeMatches(idleGate()), nil
	})
}

func (h *recoveryHarness) taintsPreserved() bool {
	node := h.node()
	if node == nil || !hasTaint(node, recovery.UnrelatedTaintKey, "keep", corev1.TaintEffectNoSchedule) {
		return false
	}
	for _, want := range h.preserved {
		if !hasTaint(node, want.Key, want.Value, want.Effect) {
			return false
		}
	}
	return true
}

func (h *recoveryHarness) snapshotNamespaces() []recovery.NamespaceSnapshot {
	var out []recovery.NamespaceSnapshot
	for _, namespace := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		list := &corev1.PodList{}
		if err := h.direct.List(h.ctx, list, client.InNamespace(namespace)); err != nil {
			h.failf("list %s: %v", namespace, err)
		}
		var uids []string
		for _, pod := range list.Items {
			uids = append(uids, string(pod.UID))
		}
		slices.Sort(uids)
		out = append(out, recovery.NamespaceSnapshot{Namespace: namespace, PodUIDs: uids})
	}
	return out
}

func (h *recoveryHarness) assertProtectedNamespaces() {
	h.t.Helper()
	if !namespaceSnapshotsEqual(h.report.ProtectedNamespaces, h.snapshotNamespaces()) {
		h.failf("protected namespace pods changed")
	}
}

func (h *recoveryHarness) assertStable() {
	h.t.Helper()
	for name, uid := range h.stableUID {
		view, ok := h.runningPod(h.ctx, name)
		if !ok || view.uid != uid {
			h.failf("protected workload %s changed from %s to %s", name, uid, view.uid)
		}
	}
}

func (h *recoveryHarness) poll(timeout time.Duration, what string, cond func(context.Context) (bool, error)) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true, cond); err != nil {
		h.failf("%s: %v (%s)", what, err, h.nodeSummary()+" "+h.conditionSummary())
	}
}

func (h *recoveryHarness) failf(format string, args ...any) {
	h.t.Helper()
	message := fmt.Sprintf(format, args...)
	if h.current != nil {
		h.current.Notes = append(h.current.Notes, message)
		h.finishScenario()
	}
	h.t.Fatalf("%s", message)
}

func (h *recoveryHarness) nodeSummary() string {
	node := h.node()
	if node == nil {
		return "node missing"
	}
	return fmt.Sprintf("taints=%v label=%s", node.Spec.Taints, node.Labels[SchedulableNodeLabel])
}

func (h *recoveryHarness) conditionSummary() string {
	reason, message := h.condition()
	return reason + ":" + message
}

func (h *recoveryHarness) reportPath() string {
	if path := os.Getenv("RECOVERY_REPORT"); path != "" {
		return path
	}
	return filepath.Join(os.TempDir(), "desktop-recovery-report.json")
}

func (h *recoveryHarness) writeReport() {
	raw, err := json.MarshalIndent(h.report, "", "  ")
	if err != nil {
		h.t.Errorf("marshal report: %v", err)
		return
	}
	if err := os.WriteFile(h.reportPath(), append(raw, '\n'), 0o644); err != nil {
		h.t.Errorf("write report: %v", err)
	}
}

func (h *recoveryHarness) finish() {
	if h.finished {
		return
	}
	h.finished = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if h.reporter != nil {
		h.reporter.shutdown()
	}
	if h.direct != nil && h.ns != "" && h.leaderA.elected != nil {
		_ = h.direct.Delete(ctx, &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: h.ns}})
		_ = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			err := h.direct.Get(ctx, types.NamespacedName{Name: "desktop", Namespace: h.ns}, &availabilityv1alpha1.NodeActivity{})
			return apierrors.IsNotFound(err), nil
		})
	}
	if h.nodeName != "" && h.direct != nil {
		_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node := &corev1.Node{}
			if err := h.direct.Get(ctx, types.NamespacedName{Name: h.nodeName}, node); err != nil {
				return err
			}
			delete(node.Labels, SchedulableNodeLabel)
			kept := node.Spec.Taints[:0]
			for _, taint := range node.Spec.Taints {
				if taint.Key == h.policy.Key || taint.Key == h.policy.BurstTaintKey || taint.Key == recovery.UnrelatedTaintKey {
					continue
				}
				kept = append(kept, taint)
			}
			node.Spec.Taints = kept
			return h.direct.Update(ctx, node)
		})
	}
	if h.leaderB != nil {
		h.leaderB.cancel()
	}
	if h.leaderA.cancel != nil {
		h.leaderA.cancel()
	}
	if h.ns != "" && h.direct != nil {
		_ = h.direct.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: h.ns}})
	}
	h.writeReport()
	if h.cancel != nil {
		h.cancel()
	}
}

func evictSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out
}

func timePtr(value time.Time) *time.Time { return &value }

func namespaceSnapshotsEqual(left, right []recovery.NamespaceSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Namespace != right[i].Namespace || !slices.Equal(left[i].PodUIDs, right[i].PodUIDs) {
			return false
		}
	}
	return true
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate module root")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
