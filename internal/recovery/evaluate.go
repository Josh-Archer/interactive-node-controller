package recovery

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const minAcceptanceGap = int64(1000)

// Evaluate checks a report against the recovery contract. An empty result
// means the evidence is internally consistent. It does not mean a Kind run
// recovered a desktop.
func Evaluate(report Report) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if report.SchemaVersion != SchemaVersion {
		add("schemaVersion = %q, want %s", report.SchemaVersion, SchemaVersion)
	}
	if report.Environment != EnvKind && report.Environment != EnvLive {
		add("environment = %q, want %s or %s", report.Environment, EnvKind, EnvLive)
	}
	if report.ClusterContext == "" || report.NodeName == "" || report.Controller == "" {
		add("clusterContext, nodeName, and controller are required")
	}
	if report.Environment == EnvKind && !strings.HasPrefix(report.ClusterContext, "kind-") {
		add("kind context %q must start with kind-", report.ClusterContext)
	}
	problems = append(problems, budgetProblems(report)...)
	problems = append(problems, scopeProblems(report)...)
	problems = append(problems, catalogProblems(report)...)
	problems = append(problems, namespaceProblems(report)...)
	if report.Environment == EnvKind && !report.UnrelatedTaintPreserved {
		add("unrelated taint %s was not preserved", UnrelatedTaintKey)
	}
	problems = append(problems, scenarioProblems(report)...)
	problems = append(problems, claimProblems(report)...)
	return problems
}

func budgetProblems(report Report) []string {
	var problems []string
	got, want := report.Budget, DefaultBudget()
	check := func(name string, got, want JSONDuration) {
		if got != want {
			problems = append(problems, fmt.Sprintf("budget.%s = %s, want %s", name, got.Duration(), want.Duration()))
		}
	}
	if got.Name != want.Name {
		problems = append(problems, fmt.Sprintf("budget.name = %q, want %q", got.Name, want.Name))
	}
	check("detection", got.Detection, want.Detection)
	check("admission", got.Admission, want.Admission)
	check("evictionAcceptance", got.EvictionAcceptance, want.EvictionAcceptance)
	check("resourceRelease", got.ResourceRelease, want.ResourceRelease)
	check("endToEnd", got.EndToEnd, want.EndToEnd)
	check("gpuRelease", got.GPURelease, want.GPURelease)
	check("failClosedSlack", got.FailClosedSlack, want.FailClosedSlack)
	check("leadApplySlack", got.LeadApplySlack, want.LeadApplySlack)
	check("leaderRestore", got.LeaderRestore, want.LeaderRestore)
	check("productionStaleAfter", got.ProductionStaleAfter, want.ProductionStaleAfter)
	check("recommendedLeadTime", got.RecommendedLeadTime, want.RecommendedLeadTime)
	if got.EndToEnd != got.ResourceRelease {
		problems = append(problems, "budget.endToEnd must equal budget.resourceRelease")
	}
	switch report.Environment {
	case EnvKind:
		check("harnessStaleAfter", got.HarnessStaleAfter, want.HarnessStaleAfter)
	case EnvLive:
		if got.HarnessStaleAfter != got.ProductionStaleAfter {
			problems = append(problems, "live budget.harnessStaleAfter must equal productionStaleAfter")
		}
	}
	return problems
}

func scopeProblems(report Report) []string {
	var problems []string
	switch report.Environment {
	case EnvKind:
		if report.DetectionScope != DetectionScopeController {
			problems = append(problems, fmt.Sprintf("kind detectionScope = %q, want %s", report.DetectionScope, DetectionScopeController))
		}
		if report.GPUMeasured {
			problems = append(problems, "kind cannot set gpuMeasured")
		}
		if report.DesktopVisible {
			problems = append(problems, "kind cannot set desktopVisible")
		}
		if report.GPUEvidence == "" || !strings.Contains(report.GPUEvidence, "no GPU") {
			problems = append(problems, "kind gpuEvidence must say no GPU was measured")
		}
		if !slices.Equal(report.LiveRunRemaining, KindLiveRunRemaining()) {
			problems = append(problems, "kind liveRunRemaining does not match the canonical live-run list")
		}
	case EnvLive:
		if report.DetectionScope != DetectionScopeHost && report.DetectionScope != DetectionScopeController {
			problems = append(problems, "live detectionScope is invalid")
		}
	}
	return problems
}

func catalogProblems(report Report) []string {
	want := Catalog()
	if len(report.WorkloadCatalog) != len(want) {
		return []string{fmt.Sprintf("workload catalog has %d entries, want %d", len(report.WorkloadCatalog), len(want))}
	}
	var problems []string
	for i, profile := range want {
		got := report.WorkloadCatalog[i]
		if got != profile {
			problems = append(problems, fmt.Sprintf("workload catalog[%d] = %+v, want %+v", i, got, profile))
		}
	}
	return problems
}

func namespaceProblems(report Report) []string {
	if report.Environment != EnvKind {
		return nil
	}
	before := indexSnapshots(report.ProtectedNamespaces)
	after := indexSnapshots(report.ProtectedNamespacesAfter)
	var problems []string
	if _, ok := before["kube-system"]; !ok {
		problems = append(problems, "protectedNamespacesBefore is missing kube-system")
	}
	for namespace, uids := range before {
		got, ok := after[namespace]
		if !ok || !slices.Equal(uids, got) {
			problems = append(problems, fmt.Sprintf("protected namespace %s pod UIDs changed", namespace))
		}
	}
	return problems
}

func indexSnapshots(snapshots []NamespaceSnapshot) map[string][]string {
	out := make(map[string][]string, len(snapshots))
	for _, snapshot := range snapshots {
		copied := append([]string(nil), snapshot.PodUIDs...)
		slices.Sort(copied)
		out[snapshot.Namespace] = copied
	}
	return out
}

func scenarioProblems(report Report) []string {
	var problems []string
	seen := map[string]ScenarioResult{}
	for _, scenario := range report.Scenarios {
		if _, ok := seen[scenario.Name]; ok {
			problems = append(problems, fmt.Sprintf("duplicate scenario %s", scenario.Name))
			continue
		}
		seen[scenario.Name] = scenario
	}
	for _, name := range RequiredScenarios() {
		scenario, ok := seen[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("missing scenario %s", name))
			continue
		}
		problems = append(problems, checkScenario(report, scenario)...)
	}
	for name := range seen {
		if !slices.Contains(RequiredScenarios(), name) {
			problems = append(problems, fmt.Sprintf("unknown scenario %s", name))
		}
	}
	return problems
}

func checkScenario(report Report, scenario ScenarioResult) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: "+format, append([]any{scenario.Name}, args...)...))
	}
	allowed := allowedTriggers(scenario.Name)
	for _, sample := range scenario.Samples {
		if !slices.Contains(allowed, sample.Trigger) {
			add("sample %s has trigger %q", sample.Phase, sample.Trigger)
		}
		problems = append(problems, checkSample(report.Budget, scenario, sample)...)
	}
	switch scenario.Name {
	case ScenarioReturnToIdle:
		problems = append(problems, requireSample(scenario, PhaseDetection, ExpectedObserved, true)...)
		problems = append(problems, requireSample(scenario, PhaseAdmissionReopen, ExpectedReopened, true)...)
		problems = append(problems, requireInactive(scenario, PhaseEviction)...)
		problems = append(problems, requireInactive(scenario, PhaseRelease)...)
	case ScenarioControllerRestart:
		problems = append(problems, requireSample(scenario, PhaseLeaderElection, ExpectedObserved, true)...)
		problems = append(problems, requireSample(scenario, PhaseLeaderRestore, ExpectedObserved, true)...)
		problems = append(problems, requireRecoveryPhases(scenario, true)...)
	case ScenarioPDB:
		problems = append(problems, requireRecoveryPhases(scenario, false)...)
		problems = append(problems, checkPDB(scenario)...)
	default:
		problems = append(problems, requireRecoveryPhases(scenario, true)...)
	}
	problems = append(problems, checkWorkloads(scenario)...)
	if scenario.Name == ScenarioScheduledLead {
		problems = append(problems, checkLead(report, scenario)...)
	} else if scenario.PreWindowReleaseProven || scenario.LeadTime != 0 || scenario.WindowStart != nil {
		add("lead fields are only valid for %s", ScenarioScheduledLead)
	}
	return problems
}

func allowedTriggers(scenario string) []string {
	switch scenario {
	case ScenarioScheduledLead:
		return []string{TriggerLeadBoundary}
	case ScenarioReporterFailure:
		return []string{TriggerReporterStopped}
	case ScenarioControllerRestart:
		return []string{TriggerLeaderCancelled, TriggerDrift, TriggerSyntheticStatus}
	case ScenarioPDB:
		return []string{TriggerSyntheticStatus, TriggerPDBDeleted}
	default:
		return []string{TriggerSyntheticStatus}
	}
}

func requireRecoveryPhases(scenario ScenarioResult, demandRelease bool) []string {
	var problems []string
	problems = append(problems, requireSample(scenario, PhaseDetection, ExpectedObserved, true)...)
	problems = append(problems, requireSample(scenario, PhaseAdmission, ExpectedObserved, true)...)
	if scenario.Name == ScenarioPDB {
		problems = append(problems, requireSample(scenario, PhaseEviction, ExpectedBlocked, true)...)
		problems = append(problems, requireSample(scenario, PhaseRelease, ExpectedBlocked, true)...)
		problems = append(problems, requireSample(scenario, PhaseEviction, ExpectedObserved, true)...)
		problems = append(problems, requireSatisfyingRelease(scenario)...)
		return problems
	}
	problems = append(problems, requireSample(scenario, PhaseEviction, ExpectedObserved, true)...)
	if demandRelease {
		problems = append(problems, requireSatisfyingRelease(scenario)...)
	}
	return problems
}

func requireSample(scenario ScenarioResult, phase, expected string, applicable bool) []string {
	for _, sample := range scenario.Samples {
		if sample.Phase == phase && sample.Expected == expected && sample.Applicable == applicable {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: missing %s sample expected=%s applicable=%v", scenario.Name, phase, expected, applicable)}
}

func requireInactive(scenario ScenarioResult, phase string) []string {
	for _, sample := range scenario.Samples {
		if sample.Phase != phase {
			continue
		}
		if sample.Applicable || sample.SatisfiesRecovery || sample.Expected != ExpectedAbsent {
			return []string{fmt.Sprintf("%s: %s must be inactive and absent", scenario.Name, phase)}
		}
		if !strings.Contains(sample.Evidence, EvidenceUIDUnchanged) {
			return []string{fmt.Sprintf("%s: %s evidence must contain %s", scenario.Name, phase, EvidenceUIDUnchanged)}
		}
		return nil
	}
	return []string{fmt.Sprintf("%s: missing inactive %s sample", scenario.Name, phase)}
}

func requireSatisfyingRelease(scenario ScenarioResult) []string {
	for _, sample := range scenario.Samples {
		if sample.Phase == PhaseRelease && sample.SatisfiesRecovery {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: resource release did not satisfy recovery", scenario.Name)}
}

func checkSample(budget Budget, scenario ScenarioResult, sample PhaseSample) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s %s: "+format, append([]any{scenario.Name, sample.Phase}, args...)...))
	}
	allowed := AllowedBudget(budget, scenario.Name, sample.Phase)
	if sample.Budget.Duration() != allowed {
		add("budget %s, want %s", sample.Budget.Duration(), allowed)
	}
	if sample.Phase == PhaseEviction && sample.SatisfiesRecovery {
		add("eviction acceptance cannot satisfy recovery")
	}
	if !sample.Applicable {
		if sample.SatisfiesRecovery || (sample.Expected != ExpectedAbsent && sample.Expected != ExpectedNotApplicable) {
			add("inactive sample is not absent")
		}
		return problems
	}
	switch sample.Expected {
	case ExpectedObserved, ExpectedReopened, ExpectedBlocked:
	default:
		add("unexpected expected value %q", sample.Expected)
		return problems
	}
	if sample.ObservedAt == nil || sample.DurationMillis == nil {
		add("missing observation timestamp")
		return problems
	}
	if sample.ObservedAt.Before(sample.TriggerAt) {
		add("observed before trigger")
	}
	got := time.Duration(*sample.DurationMillis) * time.Millisecond
	within := got <= allowed
	if sample.WithinBudget != within {
		add("withinBudget=%v but duration %s allowed %s", sample.WithinBudget, got, allowed)
	}
	if !within {
		add("duration %s exceeds %s", got, allowed)
	}
	if sample.SatisfiesRecovery {
		if sample.Phase != PhaseRelease || sample.Expected != ExpectedObserved {
			add("only an observed resource-release sample can satisfy recovery")
		}
		if !strings.Contains(sample.Evidence, EvidencePodAbsent) || !strings.Contains(sample.Evidence, EvidenceRequestsDropped) {
			add("recovery evidence requires %s and %s", EvidencePodAbsent, EvidenceRequestsDropped)
		}
		if strings.Contains(sample.Evidence, EvidenceStillBound) {
			add("recovery evidence still says %s", EvidenceStillBound)
		}
		if sample.AcceptanceGapMillis == nil || *sample.AcceptanceGapMillis < minAcceptanceGap {
			add("acceptance gap must be at least %dms so acceptance is not treated as release", minAcceptanceGap)
		}
		if got > budget.EndToEnd.Duration() {
			add("end-to-end duration %s exceeds %s", got, budget.EndToEnd.Duration())
		}
	}
	if sample.Expected == ExpectedBlocked {
		if sample.SatisfiesRecovery {
			add("blocked sample cannot satisfy recovery")
		}
		if sample.Phase == PhaseRelease && !strings.Contains(sample.Evidence, EvidenceStillBound) {
			add("blocked release must contain %s", EvidenceStillBound)
		}
		if sample.Phase == PhaseEviction && !strings.Contains(sample.Evidence, EvidenceTimestampAbsent) {
			add("blocked eviction must contain %s", EvidenceTimestampAbsent)
		}
	}
	return problems
}

func checkPDB(scenario ScenarioResult) []string {
	var problems []string
	found := false
	for _, blocked := range scenario.Blocked {
		if blocked.Workload != WorkloadPDB || blocked.Reason != "pdb" {
			continue
		}
		found = true
		if blocked.EvictionAccepted || blocked.ResourcesReleased {
			problems = append(problems, scenario.Name+": pdb blocked case must not accept eviction or release resources")
		}
		if !strings.Contains(blocked.Evidence, EvidencePDB) || !strings.Contains(blocked.Evidence, EvidenceStillBound) {
			problems = append(problems, scenario.Name+": pdb blocked evidence must include pdb-blocked and still-bound")
		}
	}
	if !found {
		problems = append(problems, scenario.Name+": missing pdb blocked case for "+WorkloadPDB)
	}
	cleared := false
	for _, sample := range scenario.Samples {
		if sample.Phase == PhaseRelease && sample.SatisfiesRecovery && sample.Trigger == TriggerPDBDeleted && strings.Contains(sample.Evidence, EvidenceAfterPDBRemoved) {
			cleared = true
		}
	}
	if !cleared {
		problems = append(problems, scenario.Name+": missing resource release after the PDB was removed")
	}
	return problems
}

func checkLead(report Report, scenario ScenarioResult) []string {
	var problems []string
	if scenario.LeadTime.Duration() <= 0 || scenario.WindowStart == nil {
		return []string{scenario.Name + ": leadTime and windowStart are required"}
	}
	if scenario.PreWindowReleaseProven {
		if scenario.LeadTime.Duration() < report.Budget.EndToEnd.Duration() {
			problems = append(problems, scenario.Name+": pre-window release requires leadTime >= endToEnd budget")
		}
		var release *PhaseSample
		for i := range scenario.Samples {
			sample := &scenario.Samples[i]
			if sample.Phase == PhaseRelease && sample.SatisfiesRecovery {
				release = sample
			}
		}
		if release == nil || release.ObservedAt == nil || !release.ObservedAt.Before(*scenario.WindowStart) {
			problems = append(problems, scenario.Name+": pre-window release is not before windowStart")
		}
	}
	return problems
}

func checkWorkloads(scenario ScenarioResult) []string {
	var problems []string
	if len(scenario.Workloads) == 0 {
		return []string{scenario.Name + ": workload observations are required"}
	}
	seenProtected := map[string]bool{}
	seen := map[string]bool{}
	for _, observation := range scenario.Workloads {
		seen[observation.Name] = true
		profile, ok := Lookup(observation.Name)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown workload %s", scenario.Name, observation.Name))
			continue
		}
		if observation.Role != profile.Role || observation.Disposable != profile.Disposable || observation.OptedIn != profile.OptedIn || observation.Lossy != profile.Lossy || observation.Pinned != profile.Pinned || observation.CPUMillis != profile.CPUMillis || observation.MemoryBytes != profile.MemoryBytes || observation.GPU != profile.GPU {
			problems = append(problems, fmt.Sprintf("%s: workload %s does not match the catalog", scenario.Name, observation.Name))
		}
		if observation.GPU != 0 {
			problems = append(problems, fmt.Sprintf("%s: workload %s requests a GPU; Kind must not", scenario.Name, observation.Name))
		}
		if observation.EvictedDuringScenario {
			if !observation.Disposable || !observation.OptedIn || observation.Pinned || observation.HoldsResourcesAfter {
				problems = append(problems, fmt.Sprintf("%s: workload %s eviction breaks the disposable opt-in contract", scenario.Name, observation.Name))
			}
		} else if observation.UIDBefore != "" && observation.UIDBefore != observation.UIDAfter {
			problems = append(problems, fmt.Sprintf("%s: workload %s changed UID without an eviction observation", scenario.Name, observation.Name))
		}
		if profile.Role == RoleProtected || profile.Pinned {
			seenProtected[observation.Name] = true
			if observation.EvictedDuringScenario || !observation.HoldsResourcesAfter || observation.UIDBefore == "" || observation.UIDBefore != observation.UIDAfter {
				problems = append(problems, fmt.Sprintf("%s: protected workload %s was touched", scenario.Name, observation.Name))
			}
		}
		wantEvict, gated := expectedEviction(scenario.Name, observation.Name, observation.Stage)
		if gated && observation.EvictedDuringScenario != wantEvict {
			problems = append(problems, fmt.Sprintf("%s: workload %s stage %q evicted=%v, want %v", scenario.Name, observation.Name, observation.Stage, observation.EvictedDuringScenario, wantEvict))
		}
		if gated && !wantEvict && !observation.HoldsResourcesAfter {
			problems = append(problems, fmt.Sprintf("%s: workload %s should still hold its requests", scenario.Name, observation.Name))
		}
	}
	for _, name := range []string{WorkloadProtected, WorkloadPinned, WorkloadLossy, WorkloadStandard} {
		if !seen[name] {
			problems = append(problems, fmt.Sprintf("%s: missing workload observation %s", scenario.Name, name))
		}
		if (name == WorkloadProtected || name == WorkloadPinned) && !seenProtected[name] {
			problems = append(problems, fmt.Sprintf("%s: missing protected observation %s", scenario.Name, name))
		}
	}
	if scenario.Name == ScenarioPDB && (!seen[WorkloadPDB] || !hasStage(scenario, "blocked") || !hasStage(scenario, "cleared")) {
		problems = append(problems, scenario.Name+": pdb-runner needs blocked and cleared observations")
	}
	return problems
}

func hasStage(scenario ScenarioResult, stage string) bool {
	for _, observation := range scenario.Workloads {
		if observation.Name == WorkloadPDB && observation.Stage == stage {
			return true
		}
	}
	return false
}

func expectedEviction(scenario, workload, stage string) (bool, bool) {
	if workload == WorkloadProtected || workload == WorkloadPinned {
		return false, true
	}
	switch scenario {
	case ScenarioIdleToInteractive, ScenarioScheduledLead, ScenarioReporterFailure:
		return workload == WorkloadLossy, workload == WorkloadLossy || workload == WorkloadStandard
	case ScenarioIdleToGame, ScenarioControllerRestart:
		return workload == WorkloadLossy || workload == WorkloadStandard, workload == WorkloadLossy || workload == WorkloadStandard
	case ScenarioReturnToIdle:
		return false, workload == WorkloadLossy || workload == WorkloadStandard
	case ScenarioPDB:
		switch workload {
		case WorkloadPDB:
			switch stage {
			case "blocked":
				return false, true
			case "cleared":
				return true, true
			default:
				return false, false
			}
		case WorkloadLossy:
			// The blocked scenario uses an interactive transition, which also
			// evicts the ordinary lossy runner. The PDB observation is separate.
			return true, true
		case WorkloadStandard:
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func claimProblems(report Report) []string {
	if !report.PerformanceClaimed {
		if report.Environment == EnvKind {
			return nil
		}
		return nil
	}
	var problems []string
	if report.Environment != EnvLive {
		problems = append(problems, "performanceClaimed is valid only for a live-gitops report")
	}
	if report.DetectionScope != DetectionScopeHost {
		problems = append(problems, "performanceClaimed requires host-to-controller detection")
	}
	if !report.GPUMeasured || !strings.Contains(strings.ToLower(report.GPUEvidence), "nvidia-smi") {
		problems = append(problems, "performanceClaimed requires measured GPU evidence including nvidia-smi")
	}
	if !report.DesktopVisible || report.DesktopEvidence == "" {
		problems = append(problems, "performanceClaimed requires desktop-visible evidence")
	}
	if len(report.LiveRunRemaining) != 0 {
		problems = append(problems, "performanceClaimed requires an empty liveRunRemaining list")
	}
	leadProven := false
	for _, scenario := range report.Scenarios {
		if scenario.Name == ScenarioScheduledLead && scenario.PreWindowReleaseProven {
			leadProven = true
		}
		for _, sample := range scenario.Samples {
			if sample.Applicable && !sample.WithinBudget && sample.Expected != ExpectedAbsent {
				problems = append(problems, fmt.Sprintf("performanceClaimed but %s %s missed its budget", scenario.Name, sample.Phase))
			}
		}
	}
	if !leadProven {
		problems = append(problems, "performanceClaimed requires pre-window release under a leadTime >= endToEnd")
	}
	return problems
}
