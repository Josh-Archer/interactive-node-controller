package controller

import (
	"context"
	"os"
	"sort"
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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestNodeActivityNodeNameIndexEnvtest(t *testing.T) {
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
	reconciler := &NodeActivityReconciler{Client: manager.GetClient(), Policy: testPolicy(), controllerName: "nodeactivity-node-index"}
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
	desktop := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
	laptop := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
	if err := direct.Create(ctx, desktop); err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, laptop); err != nil {
		t.Fatal(err)
	}
	create := func(namespace, name, nodeName string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity) *availabilityv1alpha1.NodeActivity {
		t.Helper()
		item := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName}}
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
	active := create("availability", "active", desktop.Name, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame)
	idle := create("other", "idle", desktop.Name, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	laptopActivity := create("other", "laptop", laptop.Name, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle)
	poll := func(check func() bool) {
		t.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) { return check(), nil }); err != nil {
			t.Fatal(err)
		}
	}
	enrolledNames := func(nodeName string) []string {
		t.Helper()
		var listed availabilityv1alpha1.NodeActivityList
		if err := reconciler.List(ctx, &listed, client.MatchingFields{NodeActivityNodeNameKey: nodeName}); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(listed.Items))
		for _, item := range listed.Items {
			if item.Spec.NodeName != nodeName {
				t.Fatalf("index returned %s/%s for node %s", item.Namespace, item.Name, nodeName)
			}
			names = append(names, item.Namespace+"/"+item.Name)
		}
		sort.Strings(names)
		return names
	}
	sameNames := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	poll(func() bool {
		if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
			return false
		}
		if err := direct.Get(ctx, client.ObjectKeyFromObject(idle), idle); err != nil {
			return false
		}
		if err := direct.Get(ctx, client.ObjectKeyFromObject(laptopActivity), laptopActivity); err != nil {
			return false
		}
		activeCondition := meta.FindStatusCondition(active.Status.Conditions, TaintAppliedCondition)
		idleCondition := meta.FindStatusCondition(idle.Status.Conditions, TaintAppliedCondition)
		laptopCondition := meta.FindStatusCondition(laptopActivity.Status.Conditions, TaintAppliedCondition)
		return activeCondition != nil && idleCondition != nil && laptopCondition != nil &&
			activeCondition.Reason == "DuplicateEnrollment" && idleCondition.Reason == "DuplicateEnrollment" &&
			laptopCondition.Reason == "TaintReconciled"
	})
	if err := direct.Get(ctx, types.NamespacedName{Name: desktop.Name}, desktop); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(desktop, "unrelated", "keep", corev1.TaintEffectNoSchedule) || !hasTaint(desktop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("desktop taints = %#v", desktop.Spec.Taints)
	}
	if err := direct.Get(ctx, types.NamespacedName{Name: laptop.Name}, laptop); err != nil {
		t.Fatal(err)
	}
	if hasTaint(laptop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("laptop received desktop protection: %#v", laptop.Spec.Taints)
	}
	poll(func() bool {
		return sameNames(enrolledNames(desktop.Name), []string{"availability/active", "other/idle"}) &&
			sameNames(enrolledNames(laptop.Name), []string{"other/laptop"}) &&
			len(enrolledNames("missing")) == 0
	})
	if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
		t.Fatal(err)
	}
	heartbeat := active.Status.HeartbeatAt
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &corev1.Node{}
		if err := direct.Get(ctx, types.NamespacedName{Name: desktop.Name}, fresh); err != nil {
			return err
		}
		kept := make([]corev1.Taint, 0, len(fresh.Spec.Taints))
		for _, taint := range fresh.Spec.Taints {
			if taint.Key == testPolicy().Key {
				taint.Value = "tampered"
				taint.Effect = corev1.TaintEffectPreferNoSchedule
			}
			kept = append(kept, taint)
		}
		fresh.Spec.Taints = kept
		if fresh.Labels == nil {
			fresh.Labels = map[string]string{}
		}
		fresh.Labels[SchedulableNodeLabel] = "true"
		return direct.Update(ctx, fresh)
	}); err != nil {
		t.Fatal(err)
	}
	poll(func() bool {
		if err := direct.Get(ctx, types.NamespacedName{Name: desktop.Name}, desktop); err != nil {
			return false
		}
		if _, labeled := desktop.Labels[SchedulableNodeLabel]; labeled {
			return false
		}
		return hasTaint(desktop, "unrelated", "keep", corev1.TaintEffectNoSchedule) && hasTaint(desktop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule)
	})
	if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
		t.Fatal(err)
	}
	if !active.Status.HeartbeatAt.Equal(heartbeat) {
		t.Fatalf("heartbeat changed from %s to %s", heartbeat, active.Status.HeartbeatAt)
	}
	if err := direct.Get(ctx, types.NamespacedName{Name: laptop.Name}, laptop); err != nil {
		t.Fatal(err)
	}
	if hasTaint(laptop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("node drift changed laptop: %#v", laptop.Spec.Taints)
	}
	if err := direct.Delete(ctx, idle); err != nil {
		t.Fatal(err)
	}
	poll(func() bool {
		return apierrors.IsNotFound(direct.Get(ctx, client.ObjectKeyFromObject(idle), &availabilityv1alpha1.NodeActivity{}))
	})
	poll(func() bool {
		if err := direct.Get(ctx, client.ObjectKeyFromObject(active), active); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(active.Status.Conditions, TaintAppliedCondition)
		return condition != nil && condition.Reason == "TaintReconciled" && sameNames(enrolledNames(desktop.Name), []string{"availability/active"})
	})
	if err := direct.Get(ctx, types.NamespacedName{Name: desktop.Name}, desktop); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(desktop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("survivor lost its taint: %#v", desktop.Spec.Taints)
	}
	if err := direct.Delete(ctx, active); err != nil {
		t.Fatal(err)
	}
	poll(func() bool {
		if !apierrors.IsNotFound(direct.Get(ctx, client.ObjectKeyFromObject(active), &availabilityv1alpha1.NodeActivity{})) {
			return false
		}
		if err := direct.Get(ctx, types.NamespacedName{Name: desktop.Name}, desktop); err != nil {
			return false
		}
		for _, taint := range desktop.Spec.Taints {
			if taint.Key == testPolicy().Key {
				return false
			}
		}
		return hasTaint(desktop, "unrelated", "keep", corev1.TaintEffectNoSchedule) && len(enrolledNames(desktop.Name)) == 0 && sameNames(enrolledNames(laptop.Name), []string{"other/laptop"})
	})
	if err := direct.Get(ctx, types.NamespacedName{Name: laptop.Name}, laptop); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(laptop, "unrelated", "keep", corev1.TaintEffectNoSchedule) || hasTaint(laptop, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("laptop taints after desktop deletion: %#v", laptop.Spec.Taints)
	}
}
