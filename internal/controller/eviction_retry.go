package controller

import (
	"hash/fnv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// blockedRetryDelay is the per-Pod-UID backoff plus bounded jitter.
// The API Retry-After hint raises the delay when it is longer than the
// configured backoff. Jitter never makes the retry earlier than either floor,
// and it is capped at the configured backoff so a large hint cannot be
// amplified without bound. The offset is a pure function of the Pod key, so
// the same UID keeps a stable delay and a replacement UID does not inherit it.
func blockedRetryDelay(key evictionRetryKey, policy EvictionPolicy, err error) time.Duration {
	delay := policy.RetryBackoff
	if delay < 0 {
		delay = 0
	}
	if seconds, ok := apierrors.SuggestsClientDelay(err); ok && seconds > 0 {
		hint := time.Duration(seconds) * time.Second
		if hint > delay {
			delay = hint
		}
	}
	jitterMax := policy.RetryJitter
	if jitterMax < 0 {
		jitterMax = 0
	}
	if policy.RetryBackoff > 0 && jitterMax > policy.RetryBackoff {
		jitterMax = policy.RetryBackoff
	}
	return delay + retryJitter(key, jitterMax)
}

func retryJitter(key evictionRetryKey, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key.node))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key.namespace))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key.name))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key.uid))
	span := uint64(max) + 1
	return time.Duration(hash.Sum64() % span)
}

func (s *evictionSummary) retryAfter(delay time.Duration) {
	if delay > 0 && (s.requeueAfter <= 0 || delay < s.requeueAfter) {
		s.requeueAfter = delay
	}
}
