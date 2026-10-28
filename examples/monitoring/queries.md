# Admission and eviction queries

These PromQL examples match the controller metrics. `interactive_node_controller_schedulable` and `interactive_node_controller_admission` describe this controller's fresh-idle stand-down for workloads that select `availability.interactive-node.io/schedulable`. They do not mean the kube-scheduler can place an arbitrary workload on the Node.

Copy [`prometheus-rules.yaml`](prometheus-rules.yaml) into the consuming GitOps repository for the stale reporter, invalid schedule, and prolonged blocked-eviction alerts.

## Stale or missing reporter

```promql
interactive_node_controller_heartbeat_valid == 0
```

```promql
interactive_node_controller_heartbeat_age_seconds
```

`heartbeat_age_seconds` is absent when the winning enrollment has no `heartbeatAt`. `heartbeat_valid` stays 0 in that case. `status.admission.reason` is `missing-heartbeat` or `stale-heartbeat`.

## Heartbeat ahead of the controller

```promql
interactive_node_controller_admission{reason="invalid-heartbeat"} == 1
```

```promql
interactive_node_controller_heartbeat_age_seconds < 0
```

A negative age means `status.heartbeatAt` is further ahead than `heartbeat-skew` (default 15s). `heartbeat_valid` is 0. `status.admission.heartbeatAgeSeconds` is omitted because the API schema rejects a negative age. Condition `InvalidHeartbeat` is True with reason `FutureHeartbeat`, and the message names the timestamp and the tolerance. Compare NTP on the reporter host and the controller. Eviction stays inactive until a heartbeat inside the tolerance resumes, or the report is missing, expired, or explicitly `state=stale`. Widening the tolerance extends how long a stopped future-dated heartbeat can look fresh.

## Invalid configuration

```promql
interactive_node_controller_config_invalid{reason="schedule"} == 1
```

The NodeActivity `InvalidSchedule` condition names the bad field. `status.admission.reason` is `invalid-schedule` while a fresh idle report cannot prove that every window is closed.

## Admission, burst fence, and the next change

```promql
interactive_node_controller_admission
```

```promql
interactive_node_controller_burst_taint
```

```promql
interactive_node_controller_next_transition_timestamp_seconds
```

Window start time stays on the existing series:

```promql
inc_window_active
```

```promql
inc_next_window_start_timestamp_seconds
```

`next_transition_timestamp_seconds` is the next heartbeat, hold, or window boundary that can change managed policy. It can be earlier than `inc_next_window_start_timestamp_seconds` when lead time, a window end, a hold, or the heartbeat deadline comes first. `status.admission.nextTransitionReason` says which one.

## Eviction outcome

```promql
interactive_node_controller_eviction_outcome
```

Outcomes are `disabled`, `audit-only`, `inactive`, `no-eligible-pods`, `blocked`, `budget-waiting`, and `completed`. `no-eligible-pods` means eviction is enabled or auditing and nothing passed the safety gates. It is not `disabled`.

Blocked pods, without using Pod names as labels:

```promql
interactive_node_controller_eviction_blocked
```

The `Eviction` condition uses `EvictionDisabled`, `AuditOnly`, `EvictionInactive`, `NoEligiblePods`, `EvictionBlocked`, `BudgetWaiting`, or `EvictionCompleted`. Counter `interactive_node_controller_evictions_total` keeps the same bounded `reason` values as the safety gates.
