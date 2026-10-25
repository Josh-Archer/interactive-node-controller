// Package recovery scores desktop-recovery measurements.
//
// A report records user-activity detection, admission closure, eviction
// acceptance, and CPU/memory release as separate samples. Eviction acceptance
// never satisfies recovery. Kind reports cannot claim desktop performance:
// they do not observe a host reporter, a GPU, or a desktop session.
package recovery

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const SchemaVersion = "desktop-recovery.v1"

const (
	EnvKind = "kind"
	EnvLive = "live-gitops"

	// DetectionScopeController is the Kind stand-in: the trigger is an accepted
	// NodeActivity status patch, not a desktop or host-reporter event.
	DetectionScopeController = "controller-only"
	// DetectionScopeHost is required before a report may claim recovery.
	DetectionScopeHost = "host-to-controller"

	ScenarioIdleToInteractive = "idle-to-interactive"
	ScenarioIdleToGame        = "idle-to-game"
	ScenarioReturnToIdle      = "return-to-idle"
	ScenarioScheduledLead     = "scheduled-lead-time"
	ScenarioReporterFailure   = "reporter-failure"
	ScenarioControllerRestart = "controller-restart-leader-change"
	ScenarioPDB               = "pdb-blocked-eviction"

	PhaseDetection       = "user-activity-detection"
	PhaseAdmission       = "admission-closure"
	PhaseAdmissionReopen = "admission-reopen"
	PhaseEviction        = "eviction-acceptance"
	PhaseRelease         = "resource-release"
	PhaseLeaderElection  = "leader-election"
	PhaseLeaderRestore   = "leader-restore"

	TriggerSyntheticStatus = "synthetic-status"
	TriggerLeadBoundary    = "lead-boundary"
	TriggerReporterStopped = "reporter-stopped"
	TriggerLeaderCancelled = "leader-cancelled"
	TriggerDrift           = "leader-drift"
	TriggerPDBDeleted      = "pdb-deleted"

	ExpectedObserved      = "observed"
	ExpectedReopened      = "reopened"
	ExpectedBlocked       = "blocked"
	ExpectedAbsent        = "absent"
	ExpectedNotApplicable = "not-applicable"

	EvidencePodAbsent         = "pod-absent"
	EvidenceRequestsDropped   = "requests-dropped"
	EvidenceStillBound        = "still-bound"
	EvidenceDeletionTimestamp = "deletionTimestamp"
	EvidenceEvictionAccepted  = "eviction-accepted"
	EvidenceTimestampAbsent   = "deletionTimestamp-absent"
	EvidenceUIDUnchanged      = "uid-unchanged"
	EvidenceAfterPDBRemoved   = "after-pdb-removed"
	EvidencePDB               = "pdb-blocked"

	RoleLossy     = "lossy"
	RoleStandard  = "standard"
	RoleProtected = "protected"
	RolePinned    = "pinned"
	RolePDB       = "pdb"
	RoleProbe     = "probe"

	WorkloadLossy     = "lossy-runner"
	WorkloadStandard  = "standard-runner"
	WorkloadProtected = "protected-neighbor"
	WorkloadPinned    = "pinned-neighbor"
	WorkloadPDB       = "pdb-runner"
	WorkloadProbe     = "admission-probe"

	// UnrelatedTaintKey is applied by the Kind harness and must survive every
	// reconcile. It is not a controller-owned key.
	UnrelatedTaintKey = "desktop-recovery.example/unrelated"
)

// JSONDuration encodes a time.Duration as a Go duration string.
type JSONDuration time.Duration

func (d JSONDuration) Duration() time.Duration { return time.Duration(d) }

func (d JSONDuration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", time.Duration(d))), nil
}

func (d *JSONDuration) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*d = 0
		return nil
	}
	if raw[0] != '"' {
		return fmt.Errorf("duration %s must be a string", raw)
	}
	parsed, err := time.ParseDuration(string(raw[1 : len(raw)-1]))
	if err != nil {
		return err
	}
	*d = JSONDuration(parsed)
	return nil
}

// Budget is the published desktop recovery target. HarnessStaleAfter is the
// only field a Kind run is allowed to shorten, and only to the value in
// DefaultBudget. A live run must use ProductionStaleAfter.
type Budget struct {
	Name                 string       `json:"name"`
	Detection            JSONDuration `json:"detection"`
	Admission            JSONDuration `json:"admission"`
	EvictionAcceptance   JSONDuration `json:"evictionAcceptance"`
	ResourceRelease      JSONDuration `json:"resourceRelease"`
	EndToEnd             JSONDuration `json:"endToEnd"`
	GPURelease           JSONDuration `json:"gpuRelease"`
	FailClosedSlack      JSONDuration `json:"failClosedSlack"`
	LeadApplySlack       JSONDuration `json:"leadApplySlack"`
	LeaderRestore        JSONDuration `json:"leaderRestore"`
	HarnessStaleAfter    JSONDuration `json:"harnessStaleAfter"`
	ProductionStaleAfter JSONDuration `json:"productionStaleAfter"`
	// RecommendedLeadTime is how long before a window the live enrollment
	// should arm so CPU/memory release can finish inside EndToEnd.
	RecommendedLeadTime JSONDuration `json:"recommendedLeadTime"`
}

// DefaultBudget is the target used before any performance claim.
//
// Detection covers host debounce and one reporter heartbeat (sample 5s,
// interactive debounce 2, heartbeat 15s) plus controller reconcile.
// Admission uses the same window because the taint is written in that
// reconcile. Eviction acceptance allows the chart's 30s retry backoff.
// End-to-end release adds termination grace and kubelet cleanup. GPU release
// uses the same end-to-end number and is live-only.
func DefaultBudget() Budget {
	return Budget{
		Name:                 "desktop-recovery-v1",
		Detection:            JSONDuration(45 * time.Second),
		Admission:            JSONDuration(45 * time.Second),
		EvictionAcceptance:   JSONDuration(60 * time.Second),
		ResourceRelease:      JSONDuration(90 * time.Second),
		EndToEnd:             JSONDuration(90 * time.Second),
		GPURelease:           JSONDuration(90 * time.Second),
		FailClosedSlack:      JSONDuration(45 * time.Second),
		LeadApplySlack:       JSONDuration(45 * time.Second),
		LeaderRestore:        JSONDuration(30 * time.Second),
		HarnessStaleAfter:    JSONDuration(20 * time.Second),
		ProductionStaleAfter: JSONDuration(time.Minute),
		RecommendedLeadTime:  JSONDuration(3 * time.Minute),
	}
}

// AllowedBudget is the maximum sample duration for one phase of one scenario.
// Reporter-failure detection is staleAfter plus FailClosedSlack. The Kind
// staleAfter is HarnessStaleAfter; a live run uses ProductionStaleAfter.
func AllowedBudget(budget Budget, scenario, phase string) time.Duration {
	switch phase {
	case PhaseLeaderElection, PhaseLeaderRestore:
		return budget.LeaderRestore.Duration()
	case PhaseEviction:
		return budget.EvictionAcceptance.Duration()
	case PhaseRelease:
		return budget.ResourceRelease.Duration()
	case PhaseAdmission, PhaseAdmissionReopen:
		return budget.Admission.Duration()
	case PhaseDetection:
		switch scenario {
		case ScenarioReporterFailure:
			return budget.HarnessStaleAfter.Duration() + budget.FailClosedSlack.Duration()
		case ScenarioScheduledLead:
			return budget.LeadApplySlack.Duration()
		default:
			return budget.Detection.Duration()
		}
	default:
		return 0
	}
}

// RequiredScenarios is the fixed canary matrix, in harness order.
func RequiredScenarios() []string {
	return []string{
		ScenarioIdleToInteractive,
		ScenarioReturnToIdle,
		ScenarioIdleToGame,
		ScenarioScheduledLead,
		ScenarioReporterFailure,
		ScenarioControllerRestart,
		ScenarioPDB,
	}
}

// WorkloadProfile is one canary workload. Sizes are requests, not measured
// usage. GPU is zero until a live run opts a disposable workload into a GPU.
type WorkloadProfile struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	CPU          string `json:"cpu"`
	CPUMillis    int64  `json:"cpuMillis"`
	Memory       string `json:"memory"`
	MemoryBytes  int64  `json:"memoryBytes"`
	GPU          int64  `json:"gpu"`
	Disposable   bool   `json:"disposable"`
	OptedIn      bool   `json:"optedIn"`
	Lossy        bool   `json:"lossy"`
	Pinned       bool   `json:"pinned"`
	NodeSelector bool   `json:"nodeSelector"`
}

func profile(name, role, cpu, memory string, disposable, optedIn, lossy, pinned, nodeSelector bool) WorkloadProfile {
	cpuQuantity := resource.MustParse(cpu)
	memoryQuantity := resource.MustParse(memory)
	return WorkloadProfile{
		Name:         name,
		Role:         role,
		CPU:          cpu,
		CPUMillis:    cpuQuantity.MilliValue(),
		Memory:       memory,
		MemoryBytes:  memoryQuantity.Value(),
		Disposable:   disposable,
		OptedIn:      optedIn,
		Lossy:        lossy,
		Pinned:       pinned,
		NodeSelector: nodeSelector,
	}
}

// Catalog is the only workload set the canary may create.
func Catalog() []WorkloadProfile {
	return []WorkloadProfile{
		profile(WorkloadLossy, RoleLossy, "100m", "64Mi", true, true, true, false, true),
		profile(WorkloadStandard, RoleStandard, "50m", "32Mi", true, true, false, false, false),
		profile(WorkloadProtected, RoleProtected, "200m", "128Mi", false, false, false, false, false),
		profile(WorkloadPinned, RolePinned, "20m", "16Mi", false, true, false, true, false),
		profile(WorkloadPDB, RolePDB, "80m", "48Mi", true, true, true, false, true),
		profile(WorkloadProbe, RoleProbe, "10m", "8Mi", true, false, false, false, true),
	}
}

// Lookup returns the catalog profile for a workload name.
func Lookup(name string) (WorkloadProfile, bool) {
	for _, profile := range Catalog() {
		if profile.Name == name {
			return profile, true
		}
	}
	return WorkloadProfile{}, false
}

// KindLiveRunRemaining is copied into every Kind report. A live performance
// claim has an empty list only after these items are done and recorded.
func KindLiveRunRemaining() []string {
	return []string{
		"Pin the reviewed chart version and image digest in the home repository GitOps source. This repository does not deploy it.",
		"Canary only homelabdesktop after the node and Longhorn replica-scheduling review (allowScheduling: false).",
		"Confirm the ArgoCD revision is Synced and Healthy, then confirm controller leadership and reporter heartbeats. Deployment readiness is not recovery.",
		"Drive idle-to-interactive, idle-to-game, return-to-idle, scheduled lead time, reporter failure, controller restart/leader change, and PDB-blocked eviction with the real host reporter.",
		"Measure GPU release on the desktop with the device plugin and a host sample such as nvidia-smi. Kind has no GPU.",
		"Record desktop-visible recovery separately from eviction acceptance and from Pod readiness.",
		"Use the production staleAfter from the canary values. Do not shorten it to meet the budget.",
		"Roll back through GitOps: stop eviction, delete the NodeActivity while the controller is running, then revert the home pin. Eviction acceptance is not proof the previous revision is restored.",
	}
}
