package controller

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestEvictionKindPDBSafety requires an explicitly selected Kind context. The
// fixture is disposable and the test creates all objects in its own namespace.
func TestEvictionKindPDBSafety(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" || os.Getenv("KIND_INTEGRATION") != "true" {
		t.Skip("set KIND_INTEGRATION=true and KUBECONFIG to an isolated Kind context")
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
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		t.Fatalf("Kind nodes unavailable: nodes=%d err=%v", len(nodes.Items), err)
	}
	namespace := "inc37-pdb-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := client.CoreV1().Namespaces().Delete(cleanupCtx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up isolated test namespace: %v", err)
		}
	})
	selector := map[string]string{"app": "pdb-target"}
	zero := int64(0)
	pod, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "protected", Labels: selector},
		Spec:       corev1.PodSpec{NodeName: nodes.Items[0].Name, Containers: []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.10"}}, TerminationGracePeriodSeconds: &zero},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		current, err := client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Pod did not become Ready in the isolated Kind cluster: %v", err)
	}
	minAvailable := intstr.FromInt(1)
	pdb, err := client.PolicyV1().PodDisruptionBudgets(namespace).Create(ctx, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "protect-one"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvailable, Selector: &metav1.LabelSelector{MatchLabels: selector}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		current, err := client.PolicyV1().PodDisruptionBudgets(namespace).Get(ctx, pdb.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return current.Status.ObservedGeneration >= current.Generation && current.Status.DisruptionsAllowed == 0, nil
	}); err != nil {
		t.Fatalf("PDB controller did not report zero disruptions allowed: %v", err)
	}
	adapter := KubernetesEvictionClient{Client: client}
	if err := adapter.Evict(ctx, namespace, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); !apierrors.IsTooManyRequests(err) {
		t.Fatalf("PDB-blocked eviction error = %v, want TooManyRequests", err)
	}
	if _, err := client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("PDB-blocked Pod disappeared: %v", err)
	}
	if err := client.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Evict(ctx, namespace, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("eviction after removing the test PDB: %v", err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		t.Fatalf("Pod was not removed after successful eviction: %v", err)
	}
}
