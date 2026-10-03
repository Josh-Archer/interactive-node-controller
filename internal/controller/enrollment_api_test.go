package controller

import (
	"context"
	"os"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestEnrollmentAPIIntegration(t *testing.T) {
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
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &NodeActivityReconciler{Client: manager.GetClient(), Policy: testPolicy()}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})
	startup, stopStartup := context.WithTimeout(ctx, 10*time.Second)
	defer stopStartup()
	if !manager.GetCache().WaitForCacheSync(startup) {
		t.Fatal("cache failed to sync")
	}
	for _, name := range []string{"availability", "other"} {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	create := func(namespace, name string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity) *availabilityv1alpha1.NodeActivity {
		t.Helper()
		item := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: node.Name}}
		if err := direct.Create(ctx, item); err != nil {
			t.Fatal(err)
		}
		before := item.DeepCopy()
		item.Status = availabilityv1alpha1.NodeActivityStatus{State: state, Activity: activity, HeartbeatAt: time.Now().UTC()}
		if err := direct.Status().Patch(ctx, item, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
		return item
	}
	active := create("availability", "active", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame)
	idle := create("other", "idle", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	poll := func(check func() bool) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) { return check(), nil }); err != nil {
			t.Fatal(err)
		}
	}
	poll(func() bool {
		if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
			return false
		}
		if err := direct.Get(ctx, client.ObjectKeyFromObject(idle), idle); err != nil {
			return false
		}
		a, b := meta.FindStatusCondition(active.Status.Conditions, TaintAppliedCondition), meta.FindStatusCondition(idle.Status.Conditions, TaintAppliedCondition)
		return a != nil && b != nil && a.Reason == "DuplicateEnrollment" && b.Reason == "DuplicateEnrollment"
	})
	assertProtection := func(value string) {
		t.Helper()
		if err := direct.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
			t.Fatal(err)
		}
		if !hasTaint(node, "unrelated", "keep", corev1.TaintEffectNoSchedule) {
			t.Fatal("unrelated taint changed")
		}
		if value != "" && !hasTaint(node, testPolicy().Key, value, corev1.TaintEffectNoSchedule) {
			t.Fatalf("taints=%#v", node.Spec.Taints)
		}
	}
	assertProtection("active")
	if err := direct.Delete(ctx, idle); err != nil {
		t.Fatal(err)
	}
	poll(func() bool {
		return apierrors.IsNotFound(direct.Get(ctx, client.ObjectKeyFromObject(idle), &availabilityv1alpha1.NodeActivity{}))
	})
	// The sibling watch must refresh the survivor without another heartbeat.
	poll(func() bool {
		if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(active.Status.Conditions, TaintAppliedCondition)
		return condition != nil && condition.Reason == "TaintReconciled"
	})
	assertProtection("active")
	if err := direct.Delete(ctx, active); err != nil {
		t.Fatal(err)
	}
	poll(func() bool {
		return apierrors.IsNotFound(direct.Get(ctx, client.ObjectKeyFromObject(active), &availabilityv1alpha1.NodeActivity{}))
	})
	assertProtection("")
	for _, taint := range node.Spec.Taints {
		if taint.Key == testPolicy().Key {
			t.Fatalf("final enrollment left its owned taint: %#v", node.Spec.Taints)
		}
	}
}
