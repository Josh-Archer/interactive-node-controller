package controller

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type evictionRetryKey struct {
	node, namespace, name string
	uid                   types.UID
}

func retryKey(node string, pod *corev1.Pod) evictionRetryKey {
	return evictionRetryKey{node: node, namespace: pod.Namespace, name: pod.Name, uid: pod.UID}
}

// Deadlines are process-local and protected across concurrently reconciled nodes.
// Every pass prunes expired entries, and a node pass drops disappeared Pod UIDs.
type evictionRetries struct {
	mu        sync.Mutex
	deadlines map[evictionRetryKey]time.Time
}

func (r *evictionRetries) prune(now time.Time, node string, pods []corev1.Pod) {
	live := make(map[evictionRetryKey]bool)
	for i := range pods {
		if pods[i].Spec.NodeName == node {
			live[retryKey(node, &pods[i])] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, deadline := range r.deadlines {
		if !now.Before(deadline) || (key.node == node && !live[key]) {
			delete(r.deadlines, key)
		}
	}
}

func (r *evictionRetries) remaining(key evictionRetryKey, now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if deadline := r.deadlines[key]; now.Before(deadline) {
		return deadline.Sub(now)
	}
	return 0
}

func (r *evictionRetries) block(key evictionRetryKey, deadline time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deadlines == nil {
		r.deadlines = make(map[evictionRetryKey]time.Time)
	}
	r.deadlines[key] = deadline
}

func (r *evictionRetries) forget(key evictionRetryKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deadlines, key)
}

func (s *evictionSummary) retryAfter(delay time.Duration) {
	if delay > 0 && (s.requeueAfter <= 0 || delay < s.requeueAfter) {
		s.requeueAfter = delay
	}
}
