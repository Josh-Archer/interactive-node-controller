package controller

import (
	"fmt"
	"time"
)

// DefaultHeartbeatSkew is the future lead that still counts as a fresh
// heartbeat. It absorbs ordinary NTP error between the reporter and the
// controller. Zero in TaintPolicy selects this value. A larger lead is not
// fresh and does not authorize eviction.
const DefaultHeartbeatSkew = 15 * time.Second

// heartbeatVerdict is the only freshness classification. Protection, the
// schedulable label, admission metrics, enrollment aggregation, and eviction
// all call heartbeatFreshness.
type heartbeatVerdict struct {
	Fresh   bool
	Missing bool
	Expired bool
	// Future is set when heartbeatAt is further ahead than skew. The node is
	// not fresh. The timestamp does not authorize eviction.
	Future bool
	// Lead is how far heartbeatAt sits ahead of now when Future is set.
	Lead time.Duration
	// AgeSeconds is whole seconds since heartbeatAt. Nil when the timestamp
	// is missing or future: status.admission.heartbeatAgeSeconds has minimum 0,
	// so a negative age cannot be stored. Tolerated future skew is 0.
	AgeSeconds *int64
}

func (p TaintPolicy) effectiveHeartbeatSkew() time.Duration {
	if p.HeartbeatSkew > 0 {
		return p.HeartbeatSkew
	}
	return DefaultHeartbeatSkew
}

// heartbeatFreshness reports whether at is a usable heartbeat at now.
// A timestamp is fresh when it is set, strictly younger than staleAfter, and
// no further ahead of now than skew. Equality with staleAfter is expired.
// Equality with skew is still fresh.
func heartbeatFreshness(at, now time.Time, staleAfter, skew time.Duration) heartbeatVerdict {
	if at.IsZero() {
		return heartbeatVerdict{Missing: true}
	}
	if skew < 0 {
		skew = 0
	}
	delta := now.Sub(at)
	if delta < -skew {
		return heartbeatVerdict{Future: true, Lead: -delta}
	}
	seconds := int64(delta / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	if delta >= staleAfter {
		return heartbeatVerdict{Expired: true, AgeSeconds: &seconds}
	}
	return heartbeatVerdict{Fresh: true, AgeSeconds: &seconds}
}

func futureHeartbeatMessage(at time.Time, skew time.Duration) string {
	return fmt.Sprintf("heartbeatAt %s is further ahead than heartbeat-skew %s; check NTP on the reporting host and the controller", at.UTC().Format(time.RFC3339Nano), skew)
}

// futureAgeSeconds is the Prometheus lead. The series is negative so a clock
// skew is visible beside heartbeat_valid 0. Sub-second leads still record -1
// because 0 would look like a current heartbeat. Status omits this age.
func futureAgeSeconds(lead time.Duration) int64 {
	seconds := int64(lead / time.Second)
	if lead%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return -1
	}
	return -seconds
}
