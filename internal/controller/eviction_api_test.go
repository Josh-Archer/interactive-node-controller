package controller

import (
	"context"
	"os"
	"testing"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type evictionFunc func(context.Context, string, string, metav1.DeleteOptions) error

func (f evictionFunc) Evict(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
	return f(ctx, namespace, name, options)
}

// Runs only against an isolated API server, never the user's kubeconfig.
func TestEvictionAPIPreconditions(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run isolated API-server tests")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	live, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := live.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workloads"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	adapter := KubernetesEvictionClient{Client: live}
	t.Run("unchanged-pod", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unchanged", Namespace: "workloads"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/test:unused"}}, TerminationGracePeriodSeconds: ptr(int64(0))}}
		created, err := live.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		uid, version := created.UID, created.ResourceVersion
		if err := adapter.Evict(ctx, created.Namespace, created.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}, GracePeriodSeconds: ptr(int64(0))}); err != nil {
			t.Fatal(err)
		}
	})
	for _, replacement := range []bool{true, false} {
		name := "changed-label"
		if replacement {
			name = "replaced-pod"
		}
		t.Run(name, func(t *testing.T) {
			pod := eligiblePod(name)
			pod.UID, pod.ResourceVersion = "", ""
			pod.OwnerReferences = nil
			pod.Spec.Containers = []corev1.Container{{Name: "app", Image: "example.invalid/test:unused"}}
			pod.Spec.TerminationGracePeriodSeconds = ptr(int64(0))
			inspected, err := live.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// The production cache saw this version before the concurrent mutation.
			cached := inspected.DeepCopy()
			cached.OwnerReferences = eligiblePod(name).OwnerReferences
			r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{cached})
			calls := 0
			r.Evictor = evictionFunc(func(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
				calls++
				if replacement {
					if err := live.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))}); err != nil {
						return err
					}
					next := pod.DeepCopy()
					next.Labels = nil
					if _, err := live.CoreV1().Pods(namespace).Create(ctx, next, metav1.CreateOptions{}); err != nil {
						return err
					}
				} else {
					next := inspected.DeepCopy()
					next.Labels = nil
					if _, err := live.CoreV1().Pods(namespace).Update(ctx, next, metav1.UpdateOptions{}); err != nil {
						return err
					}
				}
				return adapter.Evict(ctx, namespace, name, options)
			})
			summary, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0])
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || summary.blocked != 1 || summary.evicted != 0 {
				t.Fatalf("calls=%d summary=%#v", calls, summary)
			}
			survivor, err := live.CoreV1().Pods(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if survivor.DeletionTimestamp != nil {
				t.Fatal("concurrently changed Pod was evicted")
			}
		})
	}
}
