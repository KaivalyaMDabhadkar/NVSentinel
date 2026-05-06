# ADR-038: Health Monitors — Health Event Cancellation Rules

## Context

A fatal `HealthEvent` quarantines a node and is released only by:

1. **Manual uncordon** — operator runs `kubectl uncordon`; fault-quarantine detects the transition and calls `CancelLatestQuarantiningEvents`. See [`cancelling-breakfix.md`](../cancelling-breakfix.md).
2. **Reboot / device reset** — a monitor (typically syslog) observes the recovery and emits a healthy `HealthEvent` for the same `CheckName`, which flips `node.status.conditions` to `False` and clears the fault-quarantine annotation map.

There is no way to express that a *different* error code clears a prior fault. An operator who knows that XID 162 implies recovery from XID 163 cannot configure NVSentinel to act on that knowledge — they must wait for a device-specific recovery signal that may never fire, or uncordon manually.

This ADR adds a third path: **per-monitor cancellation rules** that declare "observation A clears observation B" pairs in configuration owned by the monitor that produces both observations.

## Problem

Two requirements shape the design:

- **The platform connector must stay domain-dumb.** Its transformer pipeline (see [ADR-023](./023-health-event-transformer-pipeline.md)) is generic in-place mutation; embedding XID semantics there would force every future check family (NIC, CSP, NVSwitch) to bake itself into the same shared layer.
- **Cancellation must reuse the existing recovery contract.** The K8s connector flips `node.status.conditions[CheckName]` only when it sees a healthy `HealthEvent` for the matching check; the fault-quarantine annotation map is keyed by `(agent, componentClass, checkName, version, nodeName, entities)` and cleared the same way. Any mechanism that does not emit such an event would leave node conditions stuck `True` while the DB row reads `Cancelled`.

The natural owner of cancellation pairs is therefore the monitor that produces the events. For XID, that is `syslog-health-monitor`: it already parses XID lines, decodes codes, resolves PCI/UUID identifiers, and constructs `EntitiesImpacted`.

## Decision

On a configured rule match, the monitor appends synthetic healthy `HealthEvent`s to the same gRPC batch alongside the original; the original is never mutated. Each synthetic event flows through the unchanged platform-connector pipeline and is folded by every downstream consumer exactly like a real recovery event. Two downstream clearers are extended to honour `ErrorCode` so a cancellation clears only its targeted code (see *Error-code precision in downstream clearers*).

Initial schema (`syslog-health-monitor`):

```toml
# health-monitors/syslog-health-monitor/.../cancellations.toml

[[checks]]
  name    = "SysLogsXIDError"
  enabled = true

  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]
```

### Synthetic event shape

For each entry in `cancelErrorCodes`:

| Field                                                                             | Value                                                               |
|-----------------------------------------------------------------------------------|---------------------------------------------------------------------|
| `Agent`, `ComponentClass`, `CheckName`, `Version`, `NodeName`, `EntitiesImpacted` | copied from the source event                                        |
| `IsHealthy`                                                                       | `true`                                                              |
| `IsFatal`                                                                         | `false`                                                             |
| `RecommendedAction`                                                               | `NONE`                                                              |
| `ErrorCode`                                                                       | the matched entry from `cancelErrorCodes`, as a single-element list |
| `Message`                                                                         | `Cancelled by <CheckName> error code <onErrorCode>`                 |
| `Metadata["nvsentinel.io/cancel-source-error-code"]`                              | the rule's `onErrorCode`                                            |
| `GeneratedTimestamp`                                                              | `time.Now()`                                                        |


The source event is unchanged; synthetic events are appended to the same `*pb.HealthEvents` batch.

## Implementation

### Config package

```plaintext
health-monitors/syslog-health-monitor/pkg/cancellation/
├── config.go     # TOML structs + loader + validation
├── resolver.go   # Per-handler errorCode → []errorCode lookup
├── config_test.go
└── resolver_test.go
```

```go
type CancellationRule struct {
    OnErrorCode      string   `toml:"onErrorCode"`
    CancelErrorCodes []string `toml:"cancelErrorCodes"`
}

type CheckCancellations struct {
    Name    string             `toml:"name"`
    Enabled bool               `toml:"enabled"`
    Rules   []CancellationRule `toml:"cancellations"`
}

type Config struct {
    Checks []CheckCancellations `toml:"checks"`
}
```

Load-time validation rejects: empty `OnErrorCode`, empty `CancelErrorCodes`, duplicate `OnErrorCode` within a check, self-cancel, and duplicate `Name` across checks.

### Resolver and handler integration

`NewResolver(cfg)` builds a `map[string][]string` from one `CheckCancellations` entry. `NewXIDHandler` gains a `*cancellation.Resolver` parameter; a `nil` resolver means the handler is unchanged from today.

Inside `createHealthEventXIDLineEvent`, after the unhealthy event is built:

```go
events := []*pb.HealthEvent{event}

if h.cancellations != nil {
    for _, target := range h.cancellations.Lookup(xidResp.Result.DecodedXIDStr) {
        events = append(events, h.buildCancellationEvent(target, entities, event))
    }
}

return &pb.HealthEvents{Version: 1, Events: events}
```

`buildCancellationEvent` constructs the synthetic event per the table above. No other call site in `XIDHandler` changes.

### Error-code precision in downstream clearers

Today the K8s connector and fault-quarantine clear by `EntitiesImpacted` only — `ErrorCode` is not consulted. Multiple XIDs on the same GPU collapse to one node-condition message stream and one annotation-map entry. A real GPU-recovery healthy event clears them all together, which is the right semantic for an actual recovery: the GPU has been reset, every prior XID on it is moot. A configured cancellation rule is different: the operator declared "XID 162 cancels XID 163" — they did **not** authorise clearing XID 98 if it happens to also be active. Without `ErrorCode` precision in the clearers, the rule would over-clear, masking unrelated faults.

This ADR therefore extends both clearers to honour `ErrorCode` when the healthy event carries one. Real GPU-recovery events (which today have empty `ErrorCode`) are unaffected — they continue to clear by entity alone, preserving today's "reset clears everything on the GPU" semantic.

**Platform connector** (`platform-connectors/pkg/connectors/kubernetes/process_node_events.go`):

`removeImpactedEntitiesMessages` becomes `removeImpactedEntitiesMessagesScoped(messages, entities, errorCodes)`. When `errorCodes` is empty, behaviour is unchanged. When non-empty, a message is removed only if it matches both an entity prefix *and* one of the supplied `ErrorCode:<code>` tokens. The healthy-event branch in `aggregateEventMessages` passes `event.ErrorCode` through.

**Fault-quarantine** (`fault-quarantine/pkg/healthEventsAnnotation/health_events_annotation_map.go`):

`HealthEventKey` (built by `CreateEventKeyForEntity`) gains an `ErrorCode` component. `createEventKeys` emits one key per `(entity, errorCode)` pair when `ErrorCode` is set, falling back to the legacy entity-only key when it is empty. `RemoveEvent` matches keys with the same fallback rule. Real recovery events (empty `ErrorCode`) match every key for the entity exactly as today; cancellation events match only the targeted code.

Both changes are additive: existing callers that produce healthy events without `ErrorCode` see no behavioural change. Unit tests in both packages cover (a) entity-only healthy event clears all error codes for the entity, (b) entity + `ErrorCode` healthy event clears only matching codes, (c) entity + non-matching `ErrorCode` clears nothing.

### Helm

```yaml
# distros/kubernetes/nvsentinel/charts/syslog-health-monitor/values.yaml

cancellations:
  - name: SysLogsXIDError
    enabled: true
    rules:
      - onErrorCode: "162"
        cancelErrorCodes: ["163"]
```

The chart's configmap renders this into `cancellations.toml`. Default values ship with no rules.

### Observability

One new counter: `syslog_health_monitor_cancellations_emitted_total{check, source_error_code, target_error_code}`, incremented per synthetic event. Downstream layers see synthetic events as ordinary healthy events and account for them through existing metrics.

## Consequences

### Positive

- Adds a declarative resolution path. The only platform-connector and fault-quarantine touch is additive `ErrorCode`-aware matching in two clearers; existing call sites are unaffected.
- **Additive, never mutating.** The original observation is left intact, so the upstream signal stays auditable and its own semantics (metrics, downstream rules keyed off the source code) are unaffected. A single source code can fan out to multiple cancellations, which mutation could not express.
- Synthetic events carry additional metadata (`Metadata["nvsentinel.io/cancel-source-error-code"]`) for correlation between the trigger and its cancellation.
- The existing hardcoded GPU-reset cancel in `createHealthEventGPUResetEvent` becomes a candidate for later migration onto this same mechanism.

### Negative

- **No cross-monitor cancellations.** A rule defined in one monitor cannot cancel another monitor's events.
- **Misconfiguration can mask faults.** Validation catches typos and self-cancels but not semantic correctness.

## Alternatives Considered

### Alternative A — Cancellation transformer in `platform-connectors`

A transformer evaluates rules and emits synthetic events into the gRPC batch.

**Rejected** because it places domain semantics on a generic-mutation layer and cannot reach `XIDHandler`'s entity resolution (PCI ↔ UUID, metadata enrichment) without leaking monitor concerns into platform-connectors.

### Alternative B — Cancellation logic in `fault-quarantine`

Extend fault-quarantine to evaluate rules and call a scoped `CancelLatestQuarantiningEvents`.

**Rejected** because fault-quarantine has no path to update `node.status.conditions` — that field is owned exclusively by the K8s connector and only flips on a matching healthy `HealthEvent`. Cancelling at this layer leaves node conditions stuck `True`.

### Alternative C — Promote each XID code to its own `CheckName`

Emit `CheckName = "GpuXid163"`, `"GpuXid162"`, etc., and use the existing per-check recovery path.

**Rejected** because every distinct XID would become its own `node.status.conditions[Type]`, fragmenting the consolidated `SysLogsXIDError` view and breaking every existing fault-quarantine rule and runbook that references it.

### Alternative D — Hardcoded XID pairs in `XIDHandler`

**Rejected** because pairs vary by driver version, hardware family, and operator policy.

## References

- [ADR-003: Rule-Based Node Quarantine](./003-rule-based-node-quarantine.md)
- [ADR-021: Health Event Property Overrides](./021-health-event-property-overrides.md)
- [ADR-023: Health Event Transformer Pipeline](./023-health-event-transformer-pipeline.md)
- [Cancelling Break-Fix Workflows](../cancelling-breakfix.md)
- `health-monitors/syslog-health-monitor/pkg/xid/xid_handler.go`
- `platform-connectors/pkg/connectors/kubernetes/process_node_events.go`
- `fault-quarantine/pkg/healthEventsAnnotation/health_events_annotation_map.go`
