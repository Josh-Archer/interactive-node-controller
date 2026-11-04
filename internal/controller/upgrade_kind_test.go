package controller

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestCRDUpgradeKind exercises upgrading from the previous-release NodeActivity
// CRD schema (v0.1.5 / v0.1.6) to the current chart CRD on a disposable Kind
// cluster. It verifies that:
//  1. Server-side applying new NodeActivity fields (spec.leadTime, spec.windows)
//     before upgrading the CRD fails schema validation (home#3733).
//  2. The controller's /readyz endpoint fails clearly when the live CRD schema
//     is older than expected.
//  3. Applying the current CRD first transitions /readyz to 200 OK and allows
//     NodeActivity resources with the new fields to apply and reconcile.
func TestCRDUpgradeKind(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" || os.Getenv("KIND_INTEGRATION") != "true" {
		t.Skip("set KIND_INTEGRATION=true and KUBECONFIG to an isolated Kind context")
	}
	rawConfig, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawConfig.CurrentContext, "kind-") {
		t.Fatalf("refusing non-Kind kubeconfig context %q", rawConfig.CurrentContext)
	}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(*rawConfig, rawConfig.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	restConfig.QPS = 50
	restConfig.Burst = 100

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		apiextensionsv1.AddToScheme,
		availabilityv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	currentCRD := loadCurrentCRDManifest(t, "chart/crds/availability.interactive-node.io_nodeactivities.yaml")
	if got := currentCRD.Annotations["argocd.argoproj.io/sync-wave"]; got != "-1" {
		t.Fatalf("chart CRD argocd.argoproj.io/sync-wave = %q, want \"-1\"", got)
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	namespace := "inc73-upgrade-" + suffix
	nodeName := "inc73-node-" + suffix

	if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()

		// Always leave the current CRD installed when the test finishes.
		_ = applyCRDToCluster(cleanupCtx, direct, currentCRD)

		activity := &availabilityv1alpha1.NodeActivity{}
		if err := direct.Get(cleanupCtx, types.NamespacedName{Namespace: namespace, Name: "workstation"}, activity); err == nil {
			if controllerutil.ContainsFinalizer(activity, NodeActivityFinalizer) {
				controllerutil.RemoveFinalizer(activity, NodeActivityFinalizer)
				_ = direct.Update(cleanupCtx, activity)
			}
			_ = direct.Delete(cleanupCtx, activity)
		}
		_ = direct.Delete(cleanupCtx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
		_ = direct.Delete(cleanupCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	})

	// 1. Install the previous release (v0.1.5) CRD schema.
	v015CRD := decodeCRDManifest(t, []byte(legacyV015NodeActivityCRD))
	if err := applyCRDToCluster(ctx, direct, v015CRD); err != nil {
		t.Fatalf("install v0.1.5 CRD: %v", err)
	}
	waitForCRDSchema(t, ctx, direct, func(crd *apiextensionsv1.CustomResourceDefinition) bool {
		return crdIsEstablished(crd) && ValidateNodeActivityCRD(crd) != nil
	})

	// Enroll a baseline NodeActivity that uses only v0.1.5 fields (spec.nodeName).
	baseline := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "workstation"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
	}
	if err := direct.Create(ctx, baseline); err != nil {
		t.Fatalf("create baseline NodeActivity on v0.1.5 CRD: %v", err)
	}

	// 2. Server-side applying a NodeActivity with new fields (spec.leadTime and
	// spec.windows) against the stale v0.1.5 CRD must fail schema validation,
	// reproducing the Argo ServerSideApply ComparisonError from home#3733.
	err = applyNodeActivityWithWindowsSSA(ctx, direct, namespace, "workstation", nodeName)
	if err == nil {
		t.Fatal("expected server-side apply with spec.windows/spec.leadTime to fail against the v0.1.5 CRD schema")
	}
	if !strings.Contains(err.Error(), "field not declared in schema") && !strings.Contains(err.Error(), "windows") && !strings.Contains(err.Error(), "leadTime") {
		t.Fatalf("unexpected server-side apply error against v0.1.5 CRD: %v", err)
	}

	// 3. Start the controller manager with the CRD schema readiness check wired
	// to /readyz and verify readiness fails clearly against v0.1.5 and v0.1.6.
	probeAddr := reserveLoopbackAddress(t)
	policy := testPolicy()
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &NodeActivityReconciler{
		Client:         manager.GetClient(),
		Policy:         policy,
		controllerName: "inc73-upgrade-" + suffix,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddReadyzCheck("crd-schema", NewCRDSchemaReadyzCheck(manager.GetAPIReader())); err != nil {
		t.Fatal(err)
	}

	mgrCtx, stopMgr := context.WithCancel(ctx)
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- manager.Start(mgrCtx) }()
	t.Cleanup(func() {
		stopMgr()
		select {
		case err := <-mgrDone:
			if err != nil {
				t.Errorf("manager exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop within timeout")
		}
	})

	readyzURL := fmt.Sprintf("http://%s/readyz", probeAddr)
	readyzDetailURL := fmt.Sprintf("http://%s/readyz/crd-schema", probeAddr)
	waitForProbeResponse(t, ctx, readyzURL, http.StatusInternalServerError, []string{
		"[-]crd-schema failed",
	})
	waitForProbeResponse(t, ctx, readyzDetailURL, http.StatusInternalServerError, []string{
		"schema is older than expected",
		"spec.leadTime",
		"spec.windows",
		"status.admission",
	})

	// Upgrade CRD only to v0.1.6 (adds spec.leadTime and spec.windows, still lacks status.admission).
	v016CRD := decodeCRDManifest(t, []byte(legacyV016NodeActivityCRD))
	if err := applyCRDToCluster(ctx, direct, v016CRD); err != nil {
		t.Fatalf("upgrade to v0.1.6 CRD: %v", err)
	}
	waitForProbeResponse(t, ctx, readyzDetailURL, http.StatusInternalServerError, []string{
		"schema is older than expected",
		"missing fields [status.admission]",
	})

	// 4. Perform the required CRD-first upgrade to the current chart CRD.
	if err := applyCRDToCluster(ctx, direct, currentCRD); err != nil {
		t.Fatalf("upgrade to current CRD: %v", err)
	}
	waitForCRDSchema(t, ctx, direct, func(crd *apiextensionsv1.CustomResourceDefinition) bool {
		return crdIsEstablished(crd) && ValidateNodeActivityCRD(crd) == nil
	})

	// Controller /readyz and /readyz/crd-schema must automatically transition to 200 OK once the CRD schema is current.
	waitForProbeResponse(t, ctx, readyzURL, http.StatusOK, []string{"ok"})
	waitForProbeResponse(t, ctx, readyzDetailURL, http.StatusOK, []string{"ok"})

	// 5. Server-side apply the NodeActivity with new fields (spec.leadTime and spec.windows)
	// now succeeds, and the controller reconciles the scheduled window and writes status.admission.
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := applyNodeActivityWithWindowsSSA(ctx, direct, namespace, "workstation", nodeName); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("server-side apply with new fields after CRD upgrade failed: %v", err)
	}

	var lastSummary string
	var generation int64
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		generation++
		now := time.Now().UTC().Format(time.RFC3339Nano)
		statusPatch := []byte(fmt.Sprintf(`{"status":{"state":"idle","activity":"idle","reason":"post-upgrade idle heartbeat","observedAt":%q,"heartbeatAt":%q,"generation":%d,"failClosed":true}}`, now, now, generation))
		target := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "workstation"}}
		_ = direct.Status().Patch(ctx, target, client.RawPatch(types.MergePatchType, statusPatch))

		updated := &availabilityv1alpha1.NodeActivity{}
		if err := direct.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "workstation"}, updated); err != nil {
			return false, nil
		}
		cond := meta.FindStatusCondition(updated.Status.Conditions, TaintAppliedCondition)
		lastSummary = fmt.Sprintf("leadTime=%q windows=%d admission=%#v cond=%#v", updated.Spec.LeadTime, len(updated.Spec.Windows), updated.Status.Admission, cond)
		if updated.Spec.LeadTime != "30m" || len(updated.Spec.Windows) != 1 {
			return false, nil
		}
		if updated.Status.Admission == nil || updated.Status.Admission.Reason != admissionScheduledGame {
			return false, nil
		}
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "ScheduledWindow" {
			return false, nil
		}
		node := &corev1.Node{}
		if err := direct.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			return false, nil
		}
		return hasTaint(node, policy.Key, policy.ActiveValue, corev1.TaintEffectNoSchedule), nil
	}); err != nil {
		t.Fatalf("controller did not reconcile new spec.windows and status.admission fields after CRD upgrade (%s): %v", lastSummary, err)
	}
}

func applyCRDToCluster(ctx context.Context, direct client.Client, desired *apiextensionsv1.CustomResourceDefinition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		crd := desired.DeepCopy()
		crd.ResourceVersion, crd.UID = "", ""
		crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{}

		existing := &apiextensionsv1.CustomResourceDefinition{}
		err := direct.Get(ctx, types.NamespacedName{Name: crd.Name}, existing)
		if apierrors.IsNotFound(err) {
			return direct.Create(ctx, crd)
		}
		if err != nil {
			return err
		}
		existing.Annotations = crd.Annotations
		existing.Labels = crd.Labels
		existing.Spec = crd.Spec
		return direct.Update(ctx, existing)
	})
}

func waitForCRDSchema(t *testing.T, ctx context.Context, direct client.Client, predicate func(*apiextensionsv1.CustomResourceDefinition) bool) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := direct.Get(ctx, types.NamespacedName{Name: NodeActivityCRDName}, crd); err != nil {
			return false, nil
		}
		return predicate(crd), nil
	}); err != nil {
		t.Fatalf("timed out waiting for CRD schema state: %v", err)
	}
}

func applyNodeActivityWithWindowsSSA(ctx context.Context, direct client.Client, namespace, name, nodeName string) error {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": availabilityv1alpha1.GroupVersion.String(),
			"kind":       availabilityv1alpha1.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"annotations": map[string]any{
					"argocd.argoproj.io/sync-wave": "1",
				},
			},
			"spec": map[string]any{
				"nodeName": nodeName,
				"leadTime": "30m",
				"windows": []any{
					map[string]any{
						"schedule": "0 * * * *",
						"duration": "1h",
						"timeZone": "UTC",
						"state":    "game",
					},
				},
			},
		},
	}
	return direct.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner("argocd-controller"))
}

func reserveLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func waitForProbeResponse(t *testing.T, ctx context.Context, url string, wantStatus int, wantSubstrings []string) {
	t.Helper()
	httpClient := &http.Client{Timeout: 2 * time.Second}
	var lastStatus int
	var lastBody string
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastBody = err.Error()
			return false, nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		lastStatus = resp.StatusCode
		lastBody = string(body)
		if resp.StatusCode != wantStatus {
			return false, nil
		}
		for _, sub := range wantSubstrings {
			if !strings.Contains(lastBody, sub) {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatalf("probe %s did not reach status %d with substrings %v (last status=%d body=%q): %v", url, wantStatus, wantSubstrings, lastStatus, lastBody, err)
	}
}
