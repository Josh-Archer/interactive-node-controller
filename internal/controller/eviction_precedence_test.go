package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A pass that is both blocked and deferred must report Blocked: classify()
// ranks a blocker above waiting for eviction budget.
func TestEvictionSummaryBlockedOutranksBudgetWaiting(t *testing.T) {
	s := evictionSummary{attempted: 2, evicted: 1, skipped: 3, blocked: 1, deferred: 2}
	s.noteBlocker("pdb-blocked", "pdb default/web blocks eviction")
	s.classify()
	if s.outcome != evictionStateBlocked {
		t.Fatalf("outcome = %q, want %q", s.outcome, evictionStateBlocked)
	}
	if strings.Contains(s.message, "waiting for eviction budget") {
		t.Fatalf("blocked message was overwritten by budget message: %q", s.message)
	}
	if s.message != "pdb default/web blocks eviction" {
		t.Fatalf("message = %q", s.message)
	}
	cond := s.condition(time.Unix(0, 0), 1)
	if cond.Status != metav1.ConditionFalse || cond.Reason != EvictionReasonBlocked {
		t.Fatalf("condition = %s/%s, want False/%s", cond.Status, cond.Reason, EvictionReasonBlocked)
	}
}
