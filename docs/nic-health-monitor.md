# NIC Health Monitor Design

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Theoretical Foundation](#2-theoretical-foundation)
3. [Architecture Overview](#3-architecture-overview)
4. [Data Structures](#4-data-structures)
5. [InfiniBand Monitoring Specification](#5-infiniband-monitoring-specification)
6. [Ethernet Monitoring Specification](#6-ethernet-monitoring-specification)
7. [Kernel Log Watcher](#7-kernel-log-watcher)
8. [Monitoring Scope and Limitations](#8-monitoring-scope-and-limitations)
9. [Configuration Schema](#9-configuration-schema)
10. [Event Management](#10-event-management)

---

## 1. Executive Summary

### 1.1 Problem Statement

Modern GPU clusters require immediate detection of **hard failures** that will cause workload crashes. The monitor focuses on deterministic fatal conditions—not gradual degradation—to ensure rapid response when a NIC failure will terminate running jobs.

### 1.2 Solution Overview

The monitor implements a **two-pipeline engine** leveraging `gpud`'s core packages:

| Pipeline | Function | Data Source | Persistence |
|----------|----------|-------------|-------------|
| **State Monitor** | Detect hard UP/DOWN transitions and fatal counters | `sysfs` state files, counters | SQLite3 (`ibPortsStore`) |
| **Log Watcher** | Detect driver/firmware failures | `/dev/kmsg` | SQLite3 (`eventstore`) |

### 1.3 Key Capabilities

1. **Deterministic failure detection** based on IBTA specifications and vendor documentation
2. **Real-time kernel log monitoring** using `gpud/pkg/kmsg` for sub-second driver/firmware failure detection
3. **Fatal counter monitoring** for counters that guarantee workload failure (e.g., `symbol_error`, `link_downed`, `excessive_buffer_overrun_errors`)
4. **Link flap detection** using `gpud`'s historical scanning logic (3 cycles of persistent downtime)
5. **Event deduplication** via `gpud/pkg/kmsg/deduper` to prevent alert storms
6. **Persistent Event Storage** in SQLite to enable stabilization windows (Sticky Window logic)

### 1.4 Fatal-Only Model

The monitor raises events **only for fatal conditions** that will cause workload failure:

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

**Fatal Event Sources:**

| Source | Fatal Conditions | Recommended Action |
|--------|------------------|--------------------|
| **State Monitor** | `state=DOWN`, `phys_state=Disabled`, `rate < target_rate` (Speed Degradation), device disappeared, PCI config=0xFF | **RecommendedAction_REPLACE_VM** |
| **Log Watcher** | `cmd_exec timeout` | **RecommendedAction_RESTART_BM** |
| **Log Watcher** | `health poll failed`, `unrecoverable`, `PCIe fatal`, `module absent` | **RecommendedAction_REPLACE_VM** |
| **Fatal Counters** | `symbol_error` (Delta > 120/hr), `local_link_integrity_errors` (Delta > 0), `excessive_buffer_overrun_errors` (Delta > 2/hour), `req_transport_retries_exceeded` (Delta > 0, Native IB) | **RecommendedAction_REPLACE_VM** |

---

## 2. Theoretical Foundation

### 2.1 The Physics of High-Speed Signaling Degradation

Modern interconnects (HDR/NDR InfiniBand, 100/200/400GbE) use **PAM4 modulation** (Pulse Amplitude Modulation, 4-level) to achieve high bandwidth. This represents a fundamental paradigm shift from previous generations.

#### 2.1.1 PAM4 vs NRZ: Why Velocity Monitoring is Required

| Aspect | NRZ (EDR/100GbE) | PAM4 (NDR/400GbE) |
|--------|------------------|-------------------|
| **Bits per symbol** | 1 | 2 |
| **Voltage levels** | 2 (0, 1) | 4 (00, 01, 10, 11) |
| **Eye height** | Maximum | **1/3 of NRZ** |
| **SNR** | High | **Drastically reduced** |
| **Raw bit errors** | Rare anomaly | **Guaranteed and constant** |
| **Monitoring approach** | "Any error is bad" | **Velocity-based only** |

> **Critical**: In PAM4 systems, raw bit errors are a **physical certainty**. A monitor that alerts on "Any Error > 0" would be permanently alarming. The velocity-based approach is the **only valid monitoring strategy** for 400G+ networks. ([Reference: PAM4 Test Challenges](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/))

**Degradation progression:**

```
Physical Impairment --> Eye Diagram Closes --> Symbol Errors --> FEC Corrections --> CRC Failures --> Packet Loss
   (cable, SFP)        (DSP struggles)      (PHY layer)       (recoverable)    (unrecoverable)
```

### 2.2 Bit Error Rate (BER), FEC, and the "Cliff Effect"

Because errors are inevitable in PAM4, **Forward Error Correction (FEC) is mandatory** for 200G/400G/NDR links.

| Link Health State | Bit Error Rate | Symbol Errors | Action |
|-------------------|----------------|---------------|--------|
| **Healthy** | < 10⁻¹⁵ | ~0 post-FEC | None |
| **Failed (Fatal)** | > 10⁻¹² | FEC margin exhausted | **Fatal (REPLACE_VM)** |

#### 2.2.1 The FEC "Cliff Effect"

FEC masks physical degradation until the error rate exceeds correction capacity—then packet loss spikes instantly from 0% to ~100%. The Degradation Monitor tracks **Pre-FEC BER** via `symbol_error` velocity, enabling node draining **before** the cliff is reached.

> **PAM4 Note (HDR/NDR)**: On 200G/400G adapters, non-zero raw BER is expected. Use `uncorrectable_fec_errors` or extreme rates (> 10⁶/sec) for fatal thresholds, not `symbol_error > 0`.

### 2.3 Driver/Firmware Interface Failures

The third failure domain is the **driver-firmware interface**. NVIDIA ConnectX NICs offload significant complexity to firmware. Operations like Queue Pair (QP) modifications involve handshakes between `mlx5_core` and device firmware.

#### 2.3.1 The Queue Pair State Machine

In the InfiniBand Verbs API, communication is established between Queue Pairs (QPs). A QP must transition through a specific state machine to become operational:

```
RESET --> INIT --> RTR --> RTS
           │        │       │
           │        │       └── Ready to Send (fully operational)
           │        └── Ready to Receive (can receive but not send)
           └── Initialize (basic parameters set)
```

The `ibv_modify_qp` call transitions the QP between states. Failures during these transitions (INIT --> RTR --> RTS) involve management packet exchanges via the Communication Manager to share keys, logical addresses (LIDs/GIDs), and capabilities.

### 2.4 Hardware Failures Detectable by Local Monitoring

This monitor focuses on **local hardware failures** that can be detected through kernel logs, hardware counters, and state changes. The following table categorizes failure scenarios:

| Failure Category | Description | Detection Method | Persistence |
|------------------|-------------|------------------|-------------|
| **Firmware/Driver Failure** | Local command interface stalled, driver crash | **Kernel Log** (`cmd_exec timeout`, `health poll failed`) | `eventstore` |
| **Physical Link Degradation** | Signal integrity issues, cable/transceiver problems | **Hardware Counters** (`symbol_error`, `link_error_recovery`) | `ibPortsStore` |
| **Link State Change** | Link down, port disabled, SM unreachable | **State Monitor** (`state`, `phys_state`, LID changes) | `ibPortsStore` |
| **PCIe/Bus Errors** | Device disappearance, AER events | **Kernel Log + State** (device removal, PCIe AER) | `eventstore` |

> **Scope Limitation**: Remote-caused failures (e.g., remote node crash, fabric black hole, switch issues) are **not detectable** from local monitoring alone and require cluster-level correlation.

#### 2.6.1 The `mlx5_core cmd_exec timeout`

This is a severe driver-level error indicating **Driver/Firmware Interface failure**. ([Reference: RHEL mlx5_core issues](https://access.redhat.com/solutions/6955682))

**Mechanism:**
1. `mlx5_core` driver sends command to NIC firmware via mailbox
2. Driver waits for firmware to toggle "Ownership bit" indicating completion
3. If firmware has crashed/hung, bit never toggles
4. Driver's watchdog expires --> logs `cmd_exec timeout` to `dmesg`

**Consequence**: Usually requires driver reload (`systemctl restart openibd`) or full node reboot. This is **always Fatal**—workload cannot proceed if it cannot issue commands to the NIC.

#### 2.6.2 Transport Retry Count Exceeded (Error 12)

When the NIC sends a packet and the ACK never arrives:

```
Send Packet --> Wait for ACK --> Timeout --> Retry (1) --> Timeout --> ... --> Retry (N) --> GIVE UP
```

After `retry_cnt` attempts (default: 7), the NIC tears down the connection and the application receives `IBV_WC_RETRY_EXC_ERR`.

**Implications:**
- Confirms **Logical Link is broken** even if physical link is UP
- Often indicates "Silent Drop" or **Black Hole** in the fabric
- Local symptom of a **remote** problem

**What This Monitor CAN Detect**: The `local_ack_timeout_err` and `req_transport_retries_exceeded` (native IB) hardware counters track these retry events at the NIC level. Rising counter values indicate transport-layer problems even if we can't see the application error.

**Correlation**: Use with [ibdiagnet](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf) to determine if issue is local (NIC) or remote (Switch/Fabric).

### 2.5 The Lossless Assumption and Deterministic Failure Horizons

Unlike general-purpose TCP/IP networks, which are architected to be resilient to packet loss, latency variation, and out-of-order delivery, RDMA fabrics—specifically InfiniBand (IB) and RDMA over Converged Ethernet (RoCE)—are designed under a **"lossless" assumption**. This architectural premise dictates that once a packet is admitted to the fabric, its delivery is guaranteed by credit-based flow control (in IB) or Priority Flow Control (in RoCE), relieving the transport layer of heavy congestion management overhead.

> **Key Insight**: This reliance on near-perfect transmission introduces a **binary fragility** to the system. When the physical or link layer violates the lossless assumption, the impact on the application is often not merely performance degradation, but **catastrophic failure**. For tightly coupled distributed workloads using MPI or NCCL, a failure in a single link **deterministically terminates the entire job**.

#### 2.5.1 Soft vs Hard Errors: The Determinism Boundary

The critical operational requirement is distinguishing between:

| Error Type | Characteristics | Impact |
|------------|-----------------|--------|
| **Soft Errors** | Probabilistic, recoverable via FEC/retries | Performance degradation, workload continues |
| **Hard Errors** | Deterministic, exceed recovery capacity | Application failure **guaranteed** |

The boundary between soft and hard errors is defined by:
1. **Counter thresholds** that indicate recovery mechanism exhaustion
2. **Rate of change** that exceeds retry bandwidth
3. **Specific counter types** that indicate fundamental violation of the lossless contract

#### 2.5.2 The 10⁻¹² BER Threshold

The InfiniBand specification defines a compliant link as maintaining a Bit Error Rate (BER) of better than **10⁻¹²**. This physical constant provides the basis for threshold calculations:

- At a BER of 10⁻¹², a link running at high speed (e.g., HDR 200Gb/s) experiences a predictable number of errors per unit time
- **IBTA-compliant threshold**: Maximum allowable symbol error rate is **120 errors per hour** ([IBTA Specification](https://www.infinibandta.org/ibta-specification/) / [Oracle Documentation](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html))
- Below this rate, FEC algorithms can typically correct errors without retransmission
- Above this rate, the "effective" error rate (post-FEC) rises, leading to packet corruption and Link Level Retransmission (LLR) or transport layer retries

> **Monitoring Implication**: While a single `SymbolError` is not fatal, a rate exceeding **120/hour** (≈2/minute) is a **deterministic predictor of impending link instability**. Monitoring systems should treat this as a **Fatal condition** requiring node replacement.

#### 2.5.3 Deterministic Failure Mechanisms

The following counters represent **absolute deterministic failure** when they increment:

| Counter | Mechanism | Why Deterministic |
|---------|-----------|-------------------|
| **link_downed** | Port Training State Machine fails to maintain LinkUp | Standard HPC applications do not support transparent dynamic rerouting of active QPs |
| **excessive_buffer_overrun_errors** | HCA internal ingress buffer overflows | Violates fundamental "lossless" contract; packet causing overrun is dropped immediately |
| **RNR_nak_retry_err** | Receiver Not Ready NAK retry exhausted | Terminal state of error handling; connection is severed |
| **local_link_integrity_errors** | Raw physical errors exceed LocalPhyErrors hardware limit | Link is operating outside design specifications |

#### 2.5.4 The Transport Layer Retry Window

When hardware counters increment, they don't directly cause application failure—they trigger a reaction in the software stack. Understanding this interaction defines the "Fatal" threshold:

```
Hardware: SymbolError --> FEC fails --> Packet Corrupted
↓
Receiver: drops packet --> PortRcvErrors increments
↓
Sender: waits for ACK --> Timeout --> Retry (1) --> ... --> Retry (N) --> GIVE UP
↓
Application: NCCL_IB_RETRY_CNT (default: 7) exhausted
↓
Result: QP transitions to ERROR state --> Application crashes
```

---

## 3. Architecture Overview

### 3.1 Design Rationale: Shared gpud Infrastructure

The monitor is built upon the `gpud` architectural framework, which utilizes a **decentralized correlation model**. Instead of a monolithic "Correlation Engine," health signals are correlated directly within the monitoring component to minimize latency and complexity.

| Correlation Strategy | mechanism | Purpose |
|----------------------|-----------|---------|
| **Temporal Correlation** | Sticky Window (10m) | Prevents "Alert Blinking" where transient recoveries hide critical issues. |
| **Log-to-Counter Correlation** | MatchFunc + Metadata | Links `/dev/kmsg` error strings to specific physical hardware paths. |
| **Velocity Correlation** | Derivative scanning | Detects flaps and drops by comparing current snapshots to historical SQLite state. |
| **Deduplication** | `kmsg/deduper` | Filters redundant kernel messages to prevent alert storms during hardware failure. |

#### 3.1.1 gpud-Inspired Data Pipeline

The monitor reuses the following core `gpud` packages to implement this correlation logic:

| Capability | gpud Package / Component | Function |
|------------|--------------------------|----------|
| **Kernel Monitoring** | `gpud/pkg/kmsg` | Real-time `/dev/kmsg` parsing and deduplication |
| **Persistence** | `gpud/pkg/sqlite` | WAL-mode SQLite for port history and event storage |
| **Event Storage** | `gpud/pkg/eventstore` | Indexed storage for correlated health events |
| **IB Discovery** | `gpud/components/.../nvidia/infiniband/class` | Unified sysfs parsing for IB/RoCE devices |

---

### 3.2 Pipeline Timing and Synchronization

| Pipeline | Mechanism | Interval | Data Target |
|----------|-----------|----------|-------------|
| **State Monitor** | Polling | 1000ms | `ibPortsStore` (SQLite) |
| **Log Watcher** | Streaming | ~100ms async | `eventstore` (SQLite) |

### 3.3 Persistence Model: The Sticky Window

Following `gpud`'s stability principles, the monitor implements a **10-minute stabilization period**:
1. When a NIC becomes unhealthy (Fatal), it is marked as such in the `eventstore`.
2. Even if the hardware counters/state recover to "Healthy", the status remains "Unhealthy" for 10 minutes.
3. This prevents "flapping health" where a transient fix hides a recurring hardware issue from operators.

### 3.4 Packet Path vs Driver/Firmware Interface Coverage

The Two-Pipeline architecture ensures **no blind spots** by covering both layers:

```
┌──────────────────────────────────────────────────────────────────┐
│                     Coverage Map                                 │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  PACKET PATH (Data Moving Through NIC):                          │
│  └── State Monitor: Link UP/DOWN, fatal counters, device loss    │
│                                                                  │
│  DRIVER/FIRMWARE INTERFACE (OS <--> NIC Communication):          │
│  └── Log Watcher: cmd_exec timeout, firmware errors, PCIe AER    │
│                                                                  │
│  Driver/firmware failures can occur while packet path appears    │
│  healthy.                                                        │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

> **Why this matters**: Simple monitors that only check link state miss Driver/Firmware Interface failures where the link appears healthy but the NIC cannot process commands.

### 3.5 Complete System Architecture

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                        NIC HEALTH MONITOR                                        │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  DATA SOURCES                                                                    │
│  ════════════                                                                    │
│  ┌───────────────────────────────────┐       ┌───────────────────────────────┐   │
│  │ /sys/class/infiniband/            │       │ /dev/kmsg                     │   │
│  │   └─ ports/<n>/                   │       │ (kernel ring buffer)          │   │
│  │      • state, phys_state, rate    │       │                               │   │
│  │      • counters/ (fatal only)     │       │ Driver messages:              │   │
│  │        - symbol_error             │       │ • mlx5_core errors            │   │
│  │        - excessive_buffer_overrun │       │ • PCIe AER events             │   │
│  │        - local_link_integrity     │       │ • firmware failures           │   │
│  │      • hw_counters/ (Native IB)   │       │ • thermal events              │   │
│  │        - req_transport_retries_exc│       │                               │   │
│  └─────────────────┬─────────────────┘       └─────────────────┬─────────────┘   │
│                    │                                           │                 │
│                    ▼                                           ▼                 │
│  ┌───────────────────────────────────┐       ┌───────────────────────────────┐   │
│  │        STATE MONITOR              │       │        LOG WATCHER            │   │
│  │        ═════════════              │       │        ═══════════            │   │
│  │                                   │       │                               │   │
│  │ Interval: 1000ms                  │       │ Interval: ~100ms (async)      │   │
│  │                                   │       │                               │   │
│  │ Detects:                          │       │ Detects:                      │   │
│  │ • Link DOWN                       │       │ • cmd_exec timeout            │   │
│  │ • Device disappeared              │       │ • health poll failed          │   │
│  │ • Physical state disabled         │       │ • unrecoverable error         │   │
│  │ • Fatal counter increment:        │       │ • PCIe fatal errors           │   │
│  │   - symbol_error                  │       │ • module absent               │   │
│  │     (Delta > 120/hr)              │       │ • high temperature            │   │
│  │   - excessive_buffer_overrun      │       │                               │   │
│  │     (Delta > 2/hour)              │       │ WHY NEEDED:                   │   │
│  │   - local_link_integrity          │       │                               │   │
│  │     (Delta > 0)                   │       │ Catches driver/firmware       │   │
│  │   - transport_retries_exc         │       │ failures when state shows     │   │
│  │     (Delta > 0)                   │       │ HEALTHY                       │   │
│  │ On disappearance:                 │       │                               │   │
│  │ --> PCI config check              │       │                               │   │
│  │   (0xFF = HW crash)               │       │                               │   │
│  │                                   │       │                               │   │
│  │ Emits: STATE_CHANGE (FATAL)       │       │ Emits: KERNEL_ERROR (FATAL)   │   │
│  └─────────────────┬─────────────────┘       └─────────────────┬─────────────┘   │
│                    │                                           │                 │
│                    └─────────────────────┬─────────────────────┘                 │
│                                          │                                       │
│                                          ▼                                       │
│                       ┌──────────────────────────────────┐                       │
│                       │       CORRELATION ENGINE         │                       │
│                       │       ══════════════════         │                       │
│                       │                                  │                       │
│                       │  • Links related events (5s)     │                       │
│                       │  • Deduplicates (60s cooldown)   │                       │
│                       │  • Identifies root cause         │                       │
│                       │  • Sticky window (10min)         │                       │
│                       └────────────────┬─────────────────┘                       │
│                                        │                                         │
│                                        │                                         │
│                                        ▼                                         │
│                             ┌─────────────────────────┐                          │
│                             │    gRPC DISPATCH        │                          │
│                             │    ════════════         │                          │
│                             │                         │                          │
│                             │ All events --> Immediate │                          │
│                             │ dispatch (FATAL only)   │                          │
│                             │                         │                          │
│                             │ --> Platform Connector   │                          │
│                             └─────────────────────────┘                          │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
```

### 3.6 Why Log Watcher is Essential

The Log Watcher detects failures that other monitors miss:

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  EXAMPLE: Firmware Failure (State Monitor sees NOTHING wrong)                │
│                                                                              │
│  State Monitor:   state=ACTIVE, phys_state=LinkUp    <-- Looks healthy!      │
│  Log Watcher:     "mlx5_core cmd_exec timeout"       <-- DETECTS FAILURE     │
│                                                                              │
│  Without Log Watcher, this node would appear healthy while unable to         │
│  process any NIC commands. Workloads would fail with mysterious timeouts.    │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 3.7 Detailed Data Flow

#### 3.7.1 State Monitor (1s interval) — FATAL FAILURES

```
Reads:
├── state          --> Logical link state (DOWN, INIT, ARMED, ACTIVE)
├── phys_state     --> Physical layer state (LinkUp, Disabled, Polling, LinkErrorRecovery)
├── operstate      --> Ethernet interface state (up, down, unknown)
├── rate           --> Negotiated link speed (e.g., 100 Gb/sec, 200 Gb/sec)
└── Fatal counters:
    ├── symbol_error                   --> Raw bit errors (IBTA BER spec)
    ├── link_downed                    --> QP disconnect
    ├── excessive_buffer_overrun_errors --> Lossless contract violated
    ├── local_link_integrity_errors    --> Link outside hardware spec
    └── req_transport_retries_exceeded --> Transport retry exhausted (Native IB only)

Detects (all FATAL):
├── Hard DOWN      --> Link completely lost, no connectivity
├── Device disappearance --> NIC no longer visible in sysfs
├── Physical disabled --> Port administratively or physically disabled
├── Link Speed Degradation --> rate < target_rate (cable/transceiver issue)
└── Fatal counter increment:
    ├── symbol_error (Delta > 120/hour)      --> IBTA BER spec exceeded
    ├── excessive_buffer_overrun_errors (Delta > 2/hour) --> Packet dropped
    ├── local_link_integrity_errors (Delta > 0) --> Hardware spec violation
    └── req_transport_retries_exceeded (Delta > 0) --> QP ERROR state (Native IB)

On device disappearance:
└── Inline PCI health check:
    ├── Config space returns 0xFF --> Hardware crash (FATAL)
    ├── Device not on PCI bus --> Clean removal (warning only)
    └── PCI read error --> Bus error (FATAL)

Emits: STATE_CHANGE events (Severity: FATAL)
```

#### 3.7.2 Log Watcher (~100ms async) — DRIVER/FIRMWARE FAILURES

```
Reads:
└── /dev/kmsg --> Kernel ring buffer (real-time streaming)

Pattern matching for (all FATAL):
├── mlx5_core cmd_exec timeout    --> Firmware communication failure
├── mlx5_core health poll failed  --> NIC health check failed
├── mlx5_core unrecoverable       --> Hardware in error state
├── PCIe fatal error              --> PCIe link broken
├── module absent                 --> Transceiver removed
├── High Temperature              --> Thermal throttling
└── pci_power_insufficient        --> Power issue

Deduplication:
└── Same message within 60s window --> Suppressed (prevents alert storms)

Emits: KERNEL_ERROR events (Severity: FATAL)
```

#### 3.7.3 Correlation Engine

```
Receives:
└── Events from both pipelines (State Monitor + Log Watcher)

Processing:
├── Temporal linking   --> Groups related events within 5s window
├── Deduplication      --> Suppresses duplicate events (60s cooldown)
├── Root cause ID      --> Correlates log messages with state changes
└── Sticky window      --> Health status remains "Unhealthy" for 10 minutes after recovery

Dispatches:
└── All FATAL events --> Immediate gRPC dispatch to Platform Connector
```

---

## 4. Data Structures

### 4.1 State Monitoring Structures (Imported from `gpud/components/accelerator/nvidia/infiniband/types`)

```go
// Represents a single port's state
type IBPort struct {
    Device           string `json:"device,omitempty"`           // e.g., "mlx5_0"
    Port             uint   `json:"port,omitempty"`             // Port number
    State            string `json:"state,omitempty"`            // e.g., "Active", "Down"
    PhysicalState    string `json:"physical_state,omitempty"`   // e.g., "LinkUp", "Disabled"
    RateGBSec        int    `json:"rate_gb_sec,omitempty"`      // e.g., 400
    LinkLayer        string `json:"link_layer,omitempty"`       // e.g., "Infiniband", "Ethernet"
    TotalLinkDowned  uint64 `json:"total_link_downed"`         // From sysfs link_downed counter
}

// Represents a NIC device
type Device struct {
    Name     string    `json:"name"`      // e.g., "mlx5_0"
    HcaType  string    `json:"hca_type"`  // e.g., "MT4123"
    FWVer    string    `json:"fw_ver"`
    Ports    []IBPort  `json:"ports"`
}
```

### 4.2 Enhanced Health Event Structure

**Design Principle**: The monitor raises events ONLY for **Fatal conditions** where the running workload (e.g., distributed training job) WILL fail or has failed.

```go
// NicHealthEvent is the internal representation that maps directly to pb.HealthEvent
type NicHealthEvent struct {
    // Core fields (map directly to pb.HealthEvent)
    Version            int32              // Protocol version (currently 1)
    Agent              string             // Agent identifier (e.g., "nic-health-monitor")
    CheckName          string             // "InfiniBandErrorCheck" or "EthernetErrorCheck"
    ComponentClass     string             // "NIC"
    GeneratedTimestamp time.Time          // Event generation time
    Message            string             // Human-readable event description
    IsFatal            bool               // TRUE = workload will/has failed
    IsHealthy          bool               // TRUE = healthy state, FALSE = unhealthy
    NodeName           string             // Kubernetes node name
    RecommendedAction  RecommendedAction  // Suggested remediation action
    EntitiesImpacted   []Entity           // List of impacted entities (NICs)

    // Internal fields (for event generation logic, not sent to protobuf)
    NicType            NicType            // Infiniband, Ethernet, RoCE
    LinkLayer          string             // "InfiniBand" or "Ethernet"
}

// Entity represents an impacted hardware entity
type Entity struct {
    EntityType  string  // e.g., "NIC"
    EntityValue string  // e.g., "mlx5_0", "mlx5_0_port1"
}

type RecommendedAction int32
const (
    RecommendedAction_NONE            RecommendedAction = 0
    RecommendedAction_COMPONENT_RESET RecommendedAction = 2
    RecommendedAction_CONTACT_SUPPORT RecommendedAction = 5
    RecommendedAction_RUN_FIELDDIAG   RecommendedAction = 6
    RecommendedAction_RESTART_VM      RecommendedAction = 15
    RecommendedAction_RESTART_BM      RecommendedAction = 24
    RecommendedAction_REPLACE_VM      RecommendedAction = 25
    RecommendedAction_RUN_DCGMEUD     RecommendedAction = 26
    RecommendedAction_UNKNOWN         RecommendedAction = 99
)


// ToHealthEvent converts internal NicHealthEvent to pb.HealthEvent for gRPC dispatch
func (e *NicHealthEvent) ToHealthEvent() *pb.HealthEvent {
    event := &pb.HealthEvent{
        Version:            e.Version,
        Agent:              e.Agent,
        CheckName:          e.CheckName,
        ComponentClass:     e.ComponentClass,
        GeneratedTimestamp: timestamppb.New(e.GeneratedTimestamp),
        Message:            e.Message,
        IsFatal:            e.IsFatal,
        IsHealthy:          e.IsHealthy,
        NodeName:           e.NodeName,
        RecommendedAction:  pb.RecommenedAction(e.RecommendedAction),
    }

    // Add impacted entities
    for _, entity := range e.EntitiesImpacted {
        event.EntitiesImpacted = append(event.EntitiesImpacted, &pb.Entity{
            EntityType:  entity.EntityType,
            EntityValue: entity.EntityValue,
        })
    }

    return event
}
```

#### 4.2.1 Fatal Condition Classification

The key question: **"Will the workload fail because of this?"**

| Condition | Recommended Action | Rationale |
|-----------|--------------------|-----------|
| **NIC state = DOWN** | **RecommendedAction_REPLACE_VM** | No network connectivity, workloads will timeout |
| **Device disappeared** | **RecommendedAction_REPLACE_VM** | Hardware failure, immediate job failure |
| **phys_state = Disabled** | **RecommendedAction_REPLACE_VM** | Port disabled, no communication possible |
| **Excessive buffer overrun** | **RecommendedAction_REPLACE_VM** | Fatal configuration error |
| **`mlx5_core unrecoverable`** | **RecommendedAction_REPLACE_VM** | Hardware in error state |
| **`mlx5_core cmd_exec timeout`** | **RecommendedAction_RESTART_BM** | Firmware/driver interface broken |
| **`health poll failed`** | **RecommendedAction_REPLACE_VM** | NIC health check failed |
| **Symbol error rate > 120/hr** | **RecommendedAction_REPLACE_VM** | Exceeds IBTA BER spec; impending link failure |
| **Local link integrity errors > 0** | **RecommendedAction_REPLACE_VM** | Physical error density exceeds hardware cap |
| **Transport retries exceeded > 0** | **RecommendedAction_REPLACE_VM** | Reliable transport failed; connection severed |
| **Link speed degradation** | **RecommendedAction_REPLACE_VM** | Negotiated rate < target; collective bottleneck |
| **Port flapping (3+ cycles)** | **RecommendedAction_REPLACE_VM** | Intermittent hardware/cable instability |

> **Note**: This monitor detects hardware-level failures through kernel events and hardware counters. Application-level errors are outside the scope of this monitor.

**Event Creation Helper:**

```go
// NewFatalEvent creates a fatal NicHealthEvent with all required fields populated
func NewFatalEvent(deviceName, message string, linkLayer string) *NicHealthEvent {
    checkName := "EthernetErrorCheck"
    if linkLayer == "InfiniBand" {
        checkName = "InfiniBandErrorCheck"
    }

    return &NicHealthEvent{
        Version:            1,
        Agent:              AGENT,
        CheckName:          checkName,
        ComponentClass:     "NIC",
        GeneratedTimestamp: time.Now(),
        Message:            message,
        IsFatal:            true,
        IsHealthy:          false,
        NodeName:           os.Getenv("NODE_NAME"),
        RecommendedAction:  RecommendedAction_NONE,
        EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: deviceName}},
        LinkLayer:          linkLayer,
    }
}

// NewHealthyEvent creates a healthy NicHealthEvent (for recovery notifications)
func NewHealthyEvent(deviceName, message string, linkLayer string) *NicHealthEvent {
    checkName := "EthernetErrorCheck"
    if linkLayer == "InfiniBand" {
        checkName = "InfiniBandErrorCheck"
    }

    return &NicHealthEvent{
        Version:            1,
        Agent:              AGENT,
        CheckName:          checkName,
        ComponentClass:     "NIC",
        GeneratedTimestamp: time.Now(),
        Message:            message,
        IsFatal:            false,
        IsHealthy:          true,
        NodeName:           os.Getenv("NODE_NAME"),
        RecommendedAction:  RecommendedAction_NONE,
        EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: deviceName}},
        LinkLayer:          linkLayer,
    }
}
```

### 4.3 Persistence Layer (SQLite with WAL)

The monitor uses **WAL-mode SQLite** via `gpud/pkg/sqlite` to maintain historical snapshots for trend analysis.

1. **`ibPortsStore`**: Stores periodic snapshots of all `IBPort` data. Reuses `gpud`'s connection management (separate Read/Write connections).
2. **`eventstore`**: Stores all generated events with a default 3-day retention.
3. **Scanning Logic**: Reuses `gpud`'s `findDrops()` and `findFlaps()` algorithms on historical snapshots.

```go
// From gpud/components/.../infiniband/store/scan_drops.go
// ib port is marked "drop" when
// 1. [state] has been "down" and has not changed, for period "threshold" (default 4m)
// 2. [totalLinkDowned] has not changed, for period "threshold"
func (ss devPortSnapshots) findDrops(device string, port uint, threshold time.Duration) []devPortSnapshotWithReason { ... }
```

### 4.4 Configuration Structure

Reuses `gpud`'s component configuration pattern for consistency.

```go
type Config struct {
    // Thresholds for health evaluation
    ExpectedPortStates []ExpectedPortStates `json:"expected_port_states"`
    
    // Retention for historical snapshots
    HistoryRetention time.Duration `json:"history_retention"`
    
    // Purge interval
    PurgeInterval time.Duration `json:"purge_interval"`
}
```

---

## 5. InfiniBand Monitoring Specification

### 5.1 Fatal Counter Reference

This monitor tracks only counters that indicate **deterministic workload failure**. All counters below are from the standard counters path: `/sys/class/infiniband/<dev>/ports/<port>/counters/`

| Counter | File Name | Failure Mechanism | Fatal Threshold | Recommended Action | Source |
|---------|-----------|-------------------|-----------------|--------------------|--------|
| **Symbol Error** | `symbol_error` | Raw bit errors at PHY layer. Exceeding IBTA BER spec (10⁻¹²) indicates link operating outside specification. | **Delta > 120/hour** | **RecommendedAction_REPLACE_VM** | [IBTA Spec](https://www.infinibandta.org/ibta-specification/) / [Oracle](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html) |
| **Local Link Integrity** | `local_link_integrity_errors` | Raw physical errors exceeded LocalPhyErrors hardware cap. Link operating outside spec. | **Delta > 0** | **RecommendedAction_REPLACE_VM** | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US) |
| **Buffer Overrun** | `excessive_buffer_overrun_errors` | HCA internal buffer overflow—**lossless contract violated**. Packet dropped immediately. | **Delta > 2/hour** | **RecommendedAction_REPLACE_VM** | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf) |
| **Transport Retries Exceeded** | `req_transport_retries_exceeded` | Transport retry limit exhausted. QP enters ERROR state, connection severed. **(Native IB only)** | **Delta > 0** | **RecommendedAction_REPLACE_VM** | [IBTA Spec](https://www.infinibandta.org/ibta-specification/) |

> **Key Design Decisions:**
> - **`symbol_error`** is **Fatal** at **Delta > 120/hour**. Per IBTA specification, this rate exceeds the maximum allowable BER of 10⁻¹² and indicates the link is operating outside spec.
> - **`local_link_integrity_errors`** is **Fatal**. This counter is a "meta-threshold"—it only increments when raw physical errors exceed the hardware-defined LocalPhyErrors cap. Link is operating outside design specifications.
> - **`excessive_buffer_overrun_errors`** uses **Delta > 2/hour threshold**. Per IBM Redbook guidance, any sustained count indicates credit-based flow control failure and packet loss.
> - **`req_transport_retries_exceeded`** is **Fatal** (Native IB only). When this increments (Delta > 0), the QP has entered ERROR state and the connection is severed. Workload failure is guaranteed.
>
> **Note on `link_downed`**: This counter is **intentionally excluded** from health monitoring. Per Oracle and HPE guidance, `link_downed` increments during valid administrative events (reboots, driver reloads) and is not a reliable indicator of hardware failure by itself. Current connectivity is monitored via `state=DOWN`.

#### 5.1.2 Counter Reset Handling

Hardware counters may reset due to driver reloads, device resets, or (rarely) uint64 overflow. The monitor must handle cases where `Current < Previous` to avoid incorrect delta calculations.

**The Problem:**
```
Poll N:   symbol_error = 1,000,000
Driver Reload / Counter Reset
Poll N+1: symbol_error = 50
Naive Delta = 50 - 1,000,000 = NEGATIVE (or overflow to huge positive)
```

**Solution:**
```go
func calculateDelta(current, previous uint64) uint64 {
    if current < previous {
        // Counter reset detected (driver reload, device reset, or overflow)
        // Treat the new value as the delta since the reset
        return current
    }
    return current - previous
}

// Usage in counter evaluation
func (m *Monitor) evaluateCounter(counterName string, current, previous uint64, thresholdPerHour float64) *NicHealthEvent {
    delta := calculateDelta(current, previous)
    
    // Calculate hourly rate based on polling interval
    hourlyRate := float64(delta) * (3600.0 / m.config.PollingIntervalSeconds)
    
    if hourlyRate > thresholdPerHour {
        return &NicHealthEvent{
            Version:            1,
            Agent:              AGENT,
            CheckName:          "InfiniBandErrorCheck",
            ComponentClass:     "NIC",
            GeneratedTimestamp: time.Now(),
            Message:            fmt.Sprintf("Counter %s exceeded threshold: %.2f/hour (limit: %.2f/hour)", 
                                           counterName, hourlyRate, thresholdPerHour),
            IsFatal:            true,
            IsHealthy:          false,
            NodeName:           os.Getenv("NODE_NAME"),
            RecommendedAction:  RecommendedAction_NONE,
            EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: deviceName}},
            LinkLayer:          "InfiniBand",
        }
    }
    return nil
}
```

**Rationale:**
- When a counter resets, the new value represents errors accumulated since the reset
- This is a conservative approach: we may slightly undercount errors immediately after a reset
- Alternative (treating reset as zero delta) could miss real errors that occurred during/after reset
- Driver reloads are logged separately by the Log Watcher, providing correlation context

> **Critical Architectural Note: RDMA vs TCP/IP Counter Domains**
>
> For RoCE devices, there are **TWO separate counter domains**:
>
> | Counter Location | Tracks | Example Traffic |
> |------------------|--------|-----------------|
> | `/sys/class/infiniband/<dev>/ports/<port>/counters/` | **RDMA traffic only** | ib_write_bw, distributed apps |
> | `/sys/class/net/<iface>/statistics/` | **TCP/IP traffic only** | ping, ssh, HTTP |
>
> **Field-validated observation**: Running `ping` through a RoCE interface does NOT increment
> InfiniBand counters (`port_rcv_data`, `port_xmit_data` stay at 0). The ping goes through the
> TCP/IP stack and is tracked in Ethernet statistics instead.
>
> **Implication for monitoring**: To detect RDMA-specific degradation (which affects distributed
> workloads), you MUST monitor the InfiniBand counters. Ethernet statistics alone will miss
> RDMA-layer issues like `rx_icrc_encapsulated` errors.

#### 5.1.1 Consolidated Deterministic Failure Thresholds

The following tables synthesize research from IBTA specifications, cloud provider heuristics (Azure, AWS), and vendor documentation (NVIDIA/Mellanox) into actionable monitoring thresholds.

This section consolidates fatal thresholds from Section 5.1 with additional context. Breaching these thresholds **guarantees application failure** or mandatory node exclusion.

| Counter Name | Type | Fatal Threshold | Recommended Action | Deterministic Mechanism | Source |
|--------------|------|-----------------|--------------------|------------------------|--------|
| `rate` (Link Speed) | State | **< target_rate** | **RecommendedAction_REPLACE_VM** | Link negotiated to lower speed due to cable/transceiver degradation. Bottlenecks collective operations. | Field experience (Section 5.2) |
| `symbol_error` | PHY | **> 120/hour** | **RecommendedAction_REPLACE_VM** | Exceeds IBTA BER spec (10⁻¹²). Link operating outside specification. | [IBTA Spec](https://www.infinibandta.org/ibta-specification/) / [Oracle](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html) |
| `link_downed` | Standard | **Delta > 0 (Runtime)** | **RecommendedAction_REPLACE_VM** | Logical path destruction; QP disconnect. Standard HPC apps don't support transparent QP rerouting. | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US) |
| `local_link_integrity_errors` | Standard | **> 0 (Any)** | **RecommendedAction_REPLACE_VM** | Physical error density exceeds hardware-defined LocalPhyErrors cap. Link outside spec. | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US) |
| `excessive_buffer_overrun_errors` | Standard | **> 2/hour** | **RecommendedAction_REPLACE_VM** | Lossless guarantee violation; packet dropped immediately. HCA ingress buffer overflow. | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf) |

**Link Flap Detection (Fatal)**

| Condition | Action | Recommended Action | Rationale |
|-----------|--------|--------------------|-----------|
| Persistent DOWN (>25s) then ACTIVE ≥ **3 times** | **Fatal: Unstable Port** | **RecommendedAction_REPLACE_VM** | `gpud` heuristic: Filters transient noise, isolates real instability. |

> **The `gpud` Flap Threshold**: A port is marked as "Flapping" (Fatal) if it exhibits **at least 3 cycles** of persistent DOWN state (> 25 seconds) followed by return to ACTIVE. Once detected, the component remains **Unhealthy (Sticky)** until administratively cleared.

### 5.2 Link Speed Degradation Detection

#### 5.2.1 The Problem

A common failure mode in GPU clusters is **link speed degradation**: a bad cable or transceiver causes the link to negotiate down (e.g., a 400Gbps NDR link training down to 200Gbps or 100Gbps). The link reports `State=Active` and `PhysState=LinkUp`, so standard state-based monitoring **will not fire**.

However, the workload (e.g., NCCL ring) will operate at a fraction of expected bandwidth, causing severe performance degradation for the entire cluster due to the slowest link bottleneck.

**Why This Happens:**
- PAM4 links (NDR/400G) have strict signal integrity requirements
- Degraded cables or dirty fiber connectors cause eye diagram closure
- Link Training State Machine (LTSM) automatically falls back to a lower speed to maintain connectivity
- The link "works" but at dramatically reduced throughput

#### 5.2.2 Detection Mechanism

**Metric:** `/sys/class/infiniband/<dev>/ports/<port>/rate`

**Logic:**
```go
func (m *Monitor) checkLinkSpeed(port IBPort) *NicHealthEvent {
    // Compare actual negotiated rate against configured target
    if port.RateGBSec < m.config.TargetRateGBSec {
        deviceName := fmt.Sprintf("%s_port%d", port.Device, port.Port)
        checkName := "InfiniBandErrorCheck"
        if port.LinkLayer == "Ethernet" {
            checkName = "EthernetErrorCheck"
        }
        return &NicHealthEvent{
            Version:            1,
            Agent:              AGENT,
            CheckName:          checkName,
            ComponentClass:     "NIC",
            GeneratedTimestamp: time.Now(),
            Message:            fmt.Sprintf("Link speed degradation: %d Gb/s (expected %d Gb/s)", 
                                           port.RateGBSec, m.config.TargetRateGBSec),
            IsFatal:            true,
            IsHealthy:          false,
            NodeName:           os.Getenv("NODE_NAME"),
            RecommendedAction:  RecommendedAction_NONE,
            EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: deviceName}},
            LinkLayer:          port.LinkLayer,
        }
    }
    return nil
}
```

**Configuration:**
```ini
[StateMonitoring]
# Expected link speed in Gb/s. Links negotiating below this are FATAL.
# Common values: 100, 200, 400 (NDR)
TargetLinkSpeedGbps = 400
```

> **Field Experience**: Link speed degradation is one of the most common undetected failure modes in GPU clusters. A single 200Gbps link in a 400Gbps fabric can reduce collective operation throughput by 50% because NCCL AllReduce operations are bounded by the slowest link.

### 5.3 State and Physical State Detection

#### Port States (Full Enumeration)

```go
const (
    // Port logical states
    IBStateDown    = "1: DOWN"      // No connectivity
    IBStateInit    = "2: INIT"      // Initializing (problematic if stuck >30s)
    IBStateArmed   = "3: ARMED"     // Armed but not active (check SM)
    IBStateActive  = "4: ACTIVE"    // Normal operational state

    // Port physical states
    IBPhysStateSleep     = "1: Sleep"
    IBPhysStatePolling   = "2: Polling"                    // Link training
    IBPhysStateDisabled  = "3: Disabled"                   // CRITICAL - port disabled
    IBPhysStateTraining  = "4: PortConfigurationTraining"  // Link negotiation
    IBPhysStateLinkUp    = "5: LinkUp"                     // Normal
    IBPhysStateLinkErr   = "6: LinkErrorRecovery"          // Active error recovery
    IBPhysStatePhyTest   = "7: Phy Test"                   // Diagnostic mode
)
```

#### State-Based Event Generation

```go
func (m *InfinibandDeviceMonitor) evaluatePortState(port InfiniBandPort) *NicHealthEvent {
    var issues []string
    severity := SeverityWarning

    // Logical state evaluation
    switch port.State {
    case IBStateDown:
        issues = append(issues, "Port DOWN - no connectivity")
        severity = SeverityCritical
    case IBStateInit:
        issues = append(issues, "Port stuck in INIT - check Subnet Manager connectivity")
        severity = SeverityWarning
    case IBStateArmed:
        issues = append(issues, "Port ARMED but not ACTIVE - check fabric manager")
        severity = SeverityWarning
    }

    // Physical state evaluation
    switch port.PhysState {
    case IBPhysStateDisabled:
        issues = append(issues, "Physical state DISABLED - check cable/switch port configuration")
        severity = SeverityCritical
    case IBPhysStatePolling:
        issues = append(issues, "Physical state POLLING - link training in progress (may indicate cable issue if persistent)")
        severity = SeverityWarning
    case IBPhysStateLinkErr:
        issues = append(issues, "Physical state LINK ERROR RECOVERY - active link problems")
        severity = SeverityCritical
    }

    if len(issues) == 0 {
        return nil
    }

    deviceName := port.DeviceName + "_" + port.PortName
    return &NicHealthEvent{
        Version:            1,
        Agent:              AGENT,
        CheckName:          "InfiniBandErrorCheck",
        ComponentClass:     "NIC",
        GeneratedTimestamp: time.Now(),
        Message:            strings.Join(issues, "; "),
        IsFatal:            severity == SeverityCritical,
        IsHealthy:          false,
        NodeName:           os.Getenv("NODE_NAME"),
        RecommendedAction:  RecommendedAction_NONE,
        EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: deviceName}},
        NicType:            Infiniband,
        LinkLayer:          "InfiniBand",
    }
}
```

### 5.4 Device Discovery and Parsing

The monitor reuses the `gpud` InfiniBand discovery package to ensure unified device identification and consistent counter mapping across `gpud` components.

**Core Package**: `gpud/components/accelerator/nvidia/infiniband/class`

This package handles:
1. Iterating over `/sys/class/infiniband`
2. Parsing `hca_type`, `fw_ver`, and `board_id`
3. Enumerating ports and reading `link_layer`, `state`, `phys_state`, and `rate`
4. Reading all `counters/` and `hw_counters/` using standard multiplication factors (e.g., 4x for data counters)

> **Consistency Note**: Using the same discovery code as `gpud` ensures that device names, port numbers, and counter values are reported identically across the system.

### 5.5 State Change Monitoring (findDrops / findFlaps)

The monitor reuses the historical scanning logic from `gpud` to distinguish between persistent port drops and transient flapping.

**Mechanism**:
1. Periodic snapshots of port state are stored in the SQLite `ibPortsStore`.
2. The scanner periodically reviews the last 10 minutes of history for each device/port.

#### 5.5.1 Port Drop Detection (`findDrops`)
An InfiniBand port is marked as "Dropped" if:
- The state has been `DOWN` for at least 4 minutes.
- The `total_link_downed` counter has NOT changed during this period (indicating it hasn't recovered).

#### 5.5.2 Port Flap Detection (`findFlaps`)
An InfiniBand port is marked as "Flapping" (**Severity: FATAL**) if:
- It has transitioned from `DOWN` to `ACTIVE` multiple times (default >= 3).
- Each `DOWN` interval lasted at least 25 seconds.
- The `total_link_downed` counter incremented with each event.

> **Effect**: Once detected, the component remains **Unhealthy (Sticky)** until administratively cleared. This heuristic filters transient noise and isolates real instability.

> **Code Reuse**: These algorithms are imported directly from `gpud/components/accelerator/nvidia/infiniband/store/scan_*.go`.

### 5.6 RoCE Handling

RoCE (RDMA over Converged Ethernet) devices appear in **both** `/sys/class/net` and `/sys/class/infiniband`. The same fatal counters apply to RoCE as to native InfiniBand:

- **Fatal counters**: `symbol_error`, `link_downed`, `local_link_integrity_errors`, `excessive_buffer_overrun_errors`, `req_transport_retries_exceeded` (Native IB only)
- **State monitoring**: `state`, `phys_state`, `operstate`
- **Kernel log patterns**: Same `mlx5_core` patterns

The monitor identifies RoCE devices by checking if the `link_layer` file contains "Ethernet".

### 5.7 NIC Vendor Detection and Mode Selection

The monitor supports multiple NIC vendors. Vendor detection determines the monitoring approach:

| Vendor | Detection | Counter Monitoring | Fatal Detection |
|--------|-----------|-------------------|-----------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - fatal counters | Counters + State + Kernel Logs |
| **AWS EFA** | Device name `rdmap*s*` | **No** | State + Kernel Logs only |

The monitor determines the NIC vendor using the following logic:
1.  Check `NICVendorMode` config.
2.  Check if device name matches `rdmap\d+s\d+` (AWS EFA).
3.  Check if device name matches `mlx5_\d+` (Mellanox).
4.  Fallback: Check driver symlink in `/sys/class/infiniband/<dev>/device/driver`.

#### AWS EFA: State-Only Monitoring

AWS EFA devices use a different architecture (SRD protocol) and do not expose the standard InfiniBand `counters/` directory. **No fatal counters are defined for EFA.**

EFA fatal detection relies on:
- **State Monitor**: Device disappearance, PCI health check (0xFF = hardware crash)
- **Kernel Log Watcher**: `efa` driver errors in `/dev/kmsg`

#### Mellanox Counter Reading

For Mellanox devices (IB and RoCE), the monitor reads:
1.  **Standard Counters**: `/sys/class/infiniband/<dev>/ports/1/counters/`
    *   Fatal counters: `symbol_error`, `link_downed`, `local_link_integrity_errors`, `excessive_buffer_overrun_errors`, `req_transport_retries_exceeded` (Native IB)
2.  **State files**: `state`, `phys_state`, `rate`

> **Note**: Mellanox throughput counters (`port_rcv_data`, `port_xmit_data`) are in 4-byte words. Multiply by 4 to get bytes.

### 5.8 Mellanox Fatal Counter Paths

For Mellanox devices (IB and RoCE), fatal counters are read from the standard counters directory:

| Counter | Path | Fatal Threshold |
|---------|------|-----------------|
| `symbol_error` | `/sys/class/infiniband/<dev>/ports/<port>/counters/symbol_error` | Delta > 120/hour |
| `local_link_integrity_errors` | `/sys/class/infiniband/<dev>/ports/<port>/counters/local_link_integrity_errors` | Delta > 0 |
| `excessive_buffer_overrun_errors` | `/sys/class/infiniband/<dev>/ports/<port>/counters/excessive_buffer_overrun_errors` | Delta > 2/hour |
| `req_transport_retries_exceeded` | `/sys/class/infiniband/<dev>/ports/<port>/hw_counters/req_transport_retries_exceeded` | Delta > 0 **(Native IB only)** |

> **Note**: The `symbol_error > 120/hour` threshold is per [IBTA specification (10⁻¹² BER)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html). `req_transport_retries_exceeded` is only available on Native InfiniBand (not RoCE). AWS EFA devices do not have the `counters/` directory and are not monitored for counter-based fatal conditions.

#### GID Table Information (RoCE Routing Diagnostics)

The GID (Global Identifier) table is critical for RoCE routing. Each device exposes GIDs at:
- `/sys/class/infiniband/<dev>/ports/<port>/gids/<index>`
- `/sys/class/infiniband/<dev>/ports/<port>/gid_attrs/types/<index>`

**GID Types discovered in production:**
- `v1` = RoCE v1 (GRH-based, layer 2)
- `v2` = RoCE v2 (UDP-encapsulated, layer 3, firewall-friendly)

**Example GID table from 34-NIC system:**
```
DEV     PORT  INDEX  GID                                      IPv4           VER   DEV
mlx5_0  1     0      fe80:0000:0000:0000:ba3f:d2ff:fec3:65c4               v1    eth0
mlx5_0  1     1      fe80:0000:0000:0000:ba3f:d2ff:fec3:65c4               v2    eth0
mlx5_0  1     2      0000:0000:0000:0000:0000:ffff:0a33:ba20  10.51.186.32 v1    eth0
mlx5_0  1     3      0000:0000:0000:0000:0000:ffff:0a33:ba20  10.51.186.32 v2    eth0
mlx5_1  1     0      fe80:0000:0000:0000:ba3f:d2ff:fe7c:7570               v1    rdma4
mlx5_1  1     2      0000:0000:0000:0000:0000:ffff:ac10:0120  172.16.1.32  v1    rdma4
```

**Diagnostic value:**
- Empty GID table --> `Error 61 (ENODATA)` during QP setup
- Missing IPv4 GIDs --> routing issues for RoCE v2
- GID type mismatch between peers --> connection failures

**Helper Functions:**
*   `getGIDCount`: Enumerates `/sys/class/infiniband/<dev>/ports/<port>/gids/` to count valid GIDs.
*   `getNetDevForIBDevice`: Discovers the network interface (e.g., `eth0`, `rdma4`) associated with an IB device by reading `/sys/class/infiniband/<dev>/device/net/`. This is critical for reading Ethernet statistics on RoCE devices.

#### SR-IOV Background: Why VFs Being DOWN is Expected

**SR-IOV (Single Root I/O Virtualization)** is a technology that allows a single physical NIC
to appear as multiple virtual NICs. Understanding this is critical for correct alerting behavior.

> **Note**: Clusters with the **NVIDIA Network Operator** installed will have SR-IOV enabled by default.

**The Problem Without Understanding SR-IOV:**
```
Monitor starts --> Sees 34 devices --> 16 are DOWN --> Generates 16 FATAL alerts!
But... those 16 devices are supposed to be DOWN. False alarm storm!
```

**How SR-IOV Works:**

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Physical NIC Card (ConnectX-6)                        │
│                                                                          │
│  ┌──────────────────┐    ┌─────────┐ ┌─────────┐ ┌─────────┐           │
│  │   PF (Physical   │    │  VF 0   │ │  VF 1   │ │  VF 2   │  ...      │
│  │    Function)     │    │(Virtual)│ │(Virtual)│ │(Virtual)│           │
│  │                  │    │         │ │         │ │         │           │
│  │ • The "real" NIC │    │ • Clone │ │ • Clone │ │ • Clone │           │
│  │ • Host OS uses   │    │ • For   │ │ • For   │ │ • For   │           │
│  │ • Always exists  │    │   VMs   │ │   VMs   │ │   VMs   │           │
│  └────────┬─────────┘    └────┬────┘ └────┬────┘ └────┬────┘           │
└───────────┼───────────────────┼──────────┼──────────┼───────────────────┘
            │                   │          │          │
            ▼                   ▼          ▼          ▼
      ┌──────────┐        ┌──────────┐ ┌──────────┐ ┌──────────┐
      │  Host OS │        │   VM 1   │ │   VM 2   │ │(Unused)  │
      │(manages) │        │(owns VF0)│ │(owns VF1)│ │   DOWN   │ ← NORMAL!
      └──────────┘        └──────────┘ └──────────┘ └──────────┘
```

**Key Terminology:**

| Term | Full Name | Description |
|------|-----------|-------------|
| **PF** | Physical Function | The "real" NIC. Host OS controls it. Should ALWAYS be ACTIVE. |
| **VF** | Virtual Function | A "virtual clone" of the PF. Created for VMs/containers to use. |

**VF Lifecycle - Why DOWN is Normal:**

```
STAGE 1: System Boot
├── PF created: mlx5_0 --> ACTIVE (host uses it)
├── VFs created: mlx5_18, mlx5_19, ... --> DOWN (waiting for assignment)
└── This is NORMAL - VFs are like empty parking spots

STAGE 2: VM Starts
├── Orchestrator assigns VF to VM: mlx5_18 --> VM1
├── mlx5_18 state changes: DOWN --> ACTIVE
└── VM now has dedicated NIC hardware

STAGE 3: VM Shuts Down
├── VF released back to pool: mlx5_18
├── mlx5_18 state changes: ACTIVE --> DOWN
└── Ready for next VM - back to "parking spot" state
```

**The Alerting Decision:**

| Device Type | State | Should Alert? | Reason |
|-------------|-------|---------------|--------|
| PF | ACTIVE | No | Normal operation |
| PF | **DOWN** | **YES!** | Real problem - host lost connectivity |
| VF | DOWN | **No** | Normal - VF not assigned to any VM |
| VF | ACTIVE | No | Normal - VF assigned and in use |

**Real Example from Field Validation (34-NIC System):**

```
┌────────────────────────────────────────────────────────────────────────┐
│  Device      State    Type   Alert if DOWN?   Reason                   │
├────────────────────────────────────────────────────────────────────────┤
│  mlx5_0      ACTIVE   PF     YES              Host management NIC      │
│  mlx5_1      ACTIVE   PF     YES              RDMA data path           │
│  ...                                                                    │
│  mlx5_17     ACTIVE   PF     YES              RDMA data path           │
│  ─────────────────────────────────────────────────────────────────────  │
│  mlx5_18     DOWN     VF     NO               Unassigned, waiting      │
│  mlx5_19     DOWN     VF     NO               Unassigned, waiting      │
│  ...                                                                    │
│  mlx5_33     DOWN     VF     NO               Unassigned, waiting      │
└────────────────────────────────────────────────────────────────────────┘
```

**Auto-Detection: How We Tell PF from VF**

The Linux kernel provides clear indicators in sysfs:

| Indicator | PF (Physical Function) | VF (Virtual Function) |
|-----------|------------------------|----------------------|
| `device/physfn` symlink | Does NOT exist | EXISTS (points to parent PF) |
| `device/sriov_totalvfs` file | EXISTS (shows max VF count) | Does NOT exist |

```
# PF Example (mlx5_0):
/sys/class/infiniband/mlx5_0/device/
├── sriov_totalvfs    ← EXISTS (value: 16 = can create 16 VFs)
└── (no physfn)       ← Doesn't exist

# VF Example (mlx5_18):
/sys/class/infiniband/mlx5_18/device/
├── (no sriov_totalvfs)  ← Doesn't exist
└── physfn --> ../0000:93:00.1  ← EXISTS (points to parent PF)
```

**Implementation:**

To determine if a `DOWN` state is expected, the monitor detects if a device is an SR-IOV Virtual Function (VF) or Physical Function (PF).
*   **Method 1 (Primary)**: Check for `physfn` symlink in the device directory. If present, it's a VF.
*   **Method 2 (Secondary)**: Check for `sriov_totalvfs` file. If present, it's a PF.

VFs are expected to be `DOWN` when unassigned. PFs are expected to be `ACTIVE`.

### 5.9 PCI Configuration Space Health Check (Inline with State Monitor)

> **Design Note**: This is NOT a separate monitoring pipeline. It is an **inline diagnostic** that runs only when the State Monitor detects a device has disappeared. This avoids redundant detection while providing root cause differentiation.

When the State Monitor detects a device has disappeared from `/sys/class/infiniband/`, we must distinguish between:
- **Clean removal**: Driver unloaded, device administratively removed
- **Dirty removal**: Hardware crash, device fell off PCIe bus

#### 5.9.1 PCI Configuration Space Validation

Reading the PCI configuration space is a standard hardware health check. If the first 64 bytes return all `0xFF`, the device has fallen off the PCIe bus.

When a device disappears from `/sys/class/infiniband`, the monitor performs an inline diagnostic:
1.  **Resolve PCI Address**: Maps the device name to its PCI address using cached `uevent` data.
2.  **Check PCI Presence**: Verifies if `/sys/bus/pci/devices/<pci_addr>` exists. If not, it's likely a clean driver unload.
3.  **Read Config Space**: Attempts to read the first 64 bytes of the PCI `config` file.
    *   **Success**: Device is present but driver is unbound (Clean Removal).
    *   **All 0xFF**: Device has fallen off the bus (Hardware Crash/Fatal Error).
    *   **Read Error**: PCI transaction failure (Fatal Error).

**Event Classification:**

| PCI Check Result | Severity | Action Required |
|------------------|----------|-----------------|
| Device not in PCI bus | Warning | Reload driver (`modprobe mlx5_core`) |
| Config space returns `0xFF` | Fatal | Re-seat card, check power, consider RMA |
| PCI read error | Fatal | Inspect PCIe slot for physical damage |

---

## 6. Ethernet Monitoring Specification

### 6.1 Fatal-Only Approach

For Ethernet interfaces, **no error counters are monitored**. All Ethernet error counters (e.g., `rx_crc_errors`, `rx_dropped`, `tx_fifo_errors`) indicate degradation but do not guarantee workload failure—TCP/IP stacks handle packet loss and retransmission transparently.

**The only fatal condition for Ethernet is:**

| Condition | Path | Detection |
|-----------|------|-----------|
| `operstate = down` | `/sys/class/net/<interface>/operstate` | State Monitor (1s polling) |

When `operstate` transitions to `down`, the interface has no connectivity and workloads will fail. This is detected by the State Monitor and emits a `STATE_CHANGE` event with `Severity: FATAL`.

> **Rationale**: Unlike RDMA fabrics (InfiniBand/RoCE) which assume lossless delivery, TCP/IP networks are designed to tolerate packet loss, retransmission, and congestion. Error counters indicate quality issues but the workload continues. Only complete link loss (`operstate=down`) is fatal.

---

## 7. Kernel Log Watcher

The monitor uses `gpud/pkg/kmsg` to stream and parse kernel messages in real-time.

### 7.1 Real-Time Streaming Architecture

The sentinel follows the `gpud` Watcher-Deduper-Syncer pattern:

1. **Watcher (`gpud/pkg/kmsg/watcher.go`)**: Tail `/dev/kmsg` and stream parsed `Message` structs (priority, sequence, timestamp, content).
2. **Deduper (`gpud/pkg/kmsg/deduper.go`)**: Aggregate identical messages within a 1-minute window to prevent alert storms.
3. **Syncer (`gpud/pkg/kmsg/syncer.go`)**: Use a `MatchFunc` to identify health-related events and insert unique matches into the persistent `eventstore`.

### 7.2 Pattern Matching (MatchFunc)

The monitor uses a component-specific `MatchFunc` (synchronized from `gpud/components/accelerator/nvidia/infiniband/kmsg_matcher.go`) to extract intent from raw kernel lines. Matches are returned as an `eventName` and a human-readable `message`.

```go
// From gpud/pkg/kmsg/syncer.go
type MatchFunc func(line string) (eventName string, message string)

// Implementation in the monitor (synced from gpud/components/accelerator/nvidia/infiniband/kmsg_matcher.go)
func Match(line string) (string, string) {
    if compiledPCIPowerInsufficient.MatchString(line) {
        return "pci_power_insufficient", "Insufficient power on MLX5 PCIe slot"
    }
    // ... additional patterns ...
    return "", ""
}
```

### 7.3 Log-to-Health Correlation mechanism

Correlation in `gpud` is **Decentralized**, **Bucket-Based**, and **Temporal**:

1.  **Bucket Isolation**: Matches from the InfiniBand `MatchFunc` are stored exclusively in the `accelerator-nvidia-infiniband` event bucket in the SQLite `eventstore`.
2.  **Evidence vs. Signal**:
    *   **Signal**: The State and Degradation pipelines determine the health status (Fatal) by polling sysfs.
    *   **Evidence**: The Log Watcher provides the "Why" (evidence) by surfacing matched kernel messages in the `Events()` API.
3.  **Temporal Proximity**: Events and State changes are correlated by the monitoring system using their timestamps. For example, a `pci_power_insufficient` log at 10:00:01 correlates with a link rate drop observed at 10:00:05.

### 7.4 Monitored Kernel Patterns

The following patterns are synchronized from `gpud/components/accelerator/nvidia/infiniband/kmsg_matcher.go`:

| Event Name | Regex Pattern | IsFatal | Recommended Action | Rationale |
|------------|---------------|---------|--------------------|-----------|
| `pci_power_insufficient` | `Detected insufficient power on the PCIe slot` | **YES** | **RecommendedAction_REPLACE_VM** | HW protection; NIC will throttle or shut down. |
| `port_module_high_temp` | `Port module event.*High Temperature` | **YES** | **RecommendedAction_REPLACE_VM** | SFP/Transceiver thermal protection. |
| `cmd_exec_timeout` | `mlx5_core.*cmd_exec timeout` | **YES** | **RecommendedAction_RESTART_BM** | Firmware hang; requires driver/node restart. |
| `unrecoverable_err` | `mlx5_core.*unrecoverable` | **YES** | **RecommendedAction_REPLACE_VM** | Hardware failure; mandatory node drain. |

---

## 8. Monitoring Scope and Limitations

### 8.1 What This Monitor CAN Detect

This monitor operates at the **driver and kernel level only**, leveraging shared `gpud` infrastructure:

| Data Source | Detection Capability | Persistence |
|-------------|---------------------|-------------|
| **sysfs state files** | Port UP/DOWN transitions, physical state changes, device disappearance | `ibPortsStore` (SQLite) |
| **sysfs counters** | Error rate degradation (symbol errors, CRC errors, congestion) | `ibPortsStore` (SQLite) |
| **sysfs hw_counters** | RDMA-layer issues (retries, timeouts, sequence errors) | `ibPortsStore` (SQLite) |
| **/dev/kmsg** | Driver errors (`mlx5_core`), firmware failures, PCIe events | `eventstore` (SQLite) |

### 8.2 What This Monitor CANNOT Detect

**Application-level logs and remote failures are out of scope:**

| Category | Examples | Why Out of Scope |
|----------|----------|------------------|
| **Application logs** | RDMA library errors, framework failures | Not in kernel ring buffer |
| **Remote node failures** | Peer node crash, peer NIC hang | No local kernel/hardware signature |
| **Fabric issues** | Switch failures, routing black holes | Requires fabric-level monitoring |
| **Subnet Manager issues** | SM unreachable (may partially detect via LID changes) | Fabric management layer, not local NIC |

### 8.3 Hardware Failures and Application Impact

The following table shows which hardware failures this monitor detects and how they may impact applications:

| Hardware Failure | Detection Method | Potential Application Impact |
|------------------|------------------|------------------------------|
| **Firmware freeze** | Kernel log: `cmd_exec timeout` | All NIC operations stall |
| **Driver crash** | Kernel log: `health poll failed` | NIC becomes unusable |
| **Physical link degradation** | Counter: `symbol_error` rising | Increased latency, packet retries |
| **Link down** | State: `state=DOWN` | All communication fails |
| **PCIe errors** | Kernel log: PCIe AER events | Device may become unavailable |
| **Transport layer issues** | Counter: `local_ack_timeout_err` | RDMA operations may timeout |

> **Key Insight**: This monitor detects **local hardware failures** through kernel logs, hardware counters, and state changes. Remote-caused failures are not detectable from local monitoring alone.

### 8.4 Hardware Failure Severity Classification

The following events indicate hardware failures that require operator attention:

| Hardware Event | Severity | Recommended Action |
|----------------|----------|-------------------|
| `mlx5_core cmd_exec timeout` | **FATAL** | **RecommendedAction_RESTART_BM** |
| `mlx5_core health poll failed` | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Port state --> DOWN | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Symbol error rate > 120/hr | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Local link integrity errors > 0 | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Transport retries exceeded > 0 | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Link speed degradation | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Port flapping (3+ cycles) | **FATAL** | **RecommendedAction_REPLACE_VM** |


---

## 9. Configuration Schema

### 9.1 Complete Configuration File

```ini
# /etc/nichealthmonitor/config.ini

#------------------------------------------------------------------------------
# General Settings
#------------------------------------------------------------------------------
[General]
# Polling interval for state monitoring (existing behavior)
PollingIntervalInMilliseconds = 1000

# Maximum time to retry before confirming NIC is down
MaxRetryDurationForDownDetectedNICInMilliseconds = 500
RetryIntervalForDownDetectedNICInMilliseconds = 100

# Network type to monitor: "all", "infiniband", "roce"
MonitorNetworkType = all

# Regex patterns for NICs to exclude (comma-separated)
NicExclusionRegex = ^veth.*,^docker.*,^br-.*,^lo$

# Regex patterns for RoCE interface filtering
# Note: Interface names are highly variable in production:
#   - eth0, eth1         : Management interfaces
#   - rdma0-15           : RDMA data interfaces
#   - nic1.0             : VLAN-tagged interfaces
#   - rdma4v0, rdma4v1   : SR-IOV Virtual Functions
RoCEInterfaceRegex = ^rdma\d+$,^eth\d+$,^nic\d+\.\d+$

#------------------------------------------------------------------------------
# NIC Vendor Mode (CRITICAL - determines counter reading strategy)
#------------------------------------------------------------------------------
# Different NIC vendors expose counters in completely different structures.
# This setting determines which code path to use for counter monitoring.
#
# Options:
#   - "auto"     : Auto-detect based on device naming and driver (RECOMMENDED)
#   - "mellanox" : Force Mellanox/NVIDIA mode (mlx5_core driver)
#                  - Uses: counters/ + hw_counters/ directories
#                  - Devices: mlx5_X naming
#                  - Supports: RoCE (Ethernet) and Native InfiniBand
#   - "efa"      : Force AWS EFA mode (efa driver)
#                  - Uses: hw_counters/ only (no counters/ directory!)
#                  - Devices: rdmapXXXsX naming
#                  - Counter units: bytes (not 4-byte words)
#
# Field-validated:
#   - Mellanox RoCE:  ConnectX-6, 100 Gb/s, 34 devices (OCI)
#   - Mellanox IB:    ConnectX-6 HDR, 200 Gb/s, 9 devices (on-prem)
#   - AWS EFA:        100 Gb/s, 32 devices (AWS p4d/p5 instances)
#
NICVendorMode = auto

# EFA-specific interface regex (for auto-detection validation)
# EFA devices use rdmapXXXsX naming convention
EFADeviceRegex = ^rdmap\d+s\d+$

# sysfs paths (for containerized deployments with host mounts)
SysClassNetPath = /sys/class/net
SysClassInfinibandPath = /sys/class/infiniband

#------------------------------------------------------------------------------
# State Monitoring Configuration
#------------------------------------------------------------------------------
[StateMonitoring]
# Expected link speed in Gb/s. Links negotiating below this are FATAL.
# A bad cable often causes a link to negotiate down (e.g., 400Gbps NDR link 
# training to 200Gbps). The link is "Active" and "Up", but workload performance
# is severely degraded (slowest link bottleneck in NCCL rings).
#
# Common values:
#   - 100  : HDR100/100GbE
#   - 200  : HDR/200GbE
#   - 400  : NDR/400GbE
#
# Set to 0 to disable speed checking (not recommended for production)
TargetLinkSpeedGbps = 400

# SR-IOV Virtual Function Auto-Detection (RECOMMENDED - no manual config needed)
# Note: Clusters with the NVIDIA Network Operator installed will have SR-IOV enabled by default.
# VFs are expected to be DOWN when not assigned to a VM/container.
# When enabled, the monitor auto-detects VFs using kernel sysfs indicators:
#
#   Detection Method 1: physfn symlink
#     - Path: /sys/class/infiniband/<dev>/device/physfn
#     - VFs HAVE this symlink (points to parent PF)
#     - PFs do NOT have this symlink
#
#   Detection Method 2: sriov_totalvfs file
#     - Path: /sys/class/infiniband/<dev>/device/sriov_totalvfs
#     - PFs HAVE this file (shows max VFs the PF can create)
#     - VFs do NOT have this file
#
# Field-validated result on 34-NIC system:
#   - 18 PFs (mlx5_0-17): ACTIVE, no physfn, has sriov_totalvfs --> Alert if DOWN
#   - 16 VFs (mlx5_18-33): DOWN, has physfn, no sriov_totalvfs --> Ignore (expected)
#
AutoDetectSRIOVVFs = true

# Manual override (OPTIONAL - only for non-SRIOV edge cases)
# Use only if you have admin-disabled PFs that should not alert.
# Comma-separated device names or regex pattern.
# ExpectedDownDevices = mlx5_disabled_port
# ExpectedDownDevicesRegex = ^mlx5_maintenance_.*$

#------------------------------------------------------------------------------
# Fatal Counter Thresholds (FATAL events only)
#------------------------------------------------------------------------------
[FatalCounterThresholds]
# These counters trigger FATAL events when thresholds are breached
# All other counters are NOT monitored (fatal-only model)
# Note: Only applies to Mellanox (IB/RoCE). AWS EFA uses state-only monitoring.

# InfiniBand/RoCE Fatal Counters (Mellanox only)
SymbolErrorPerHour = 120           # FATAL: Delta > 120/hour (per IBTA BER spec 10^-12)
ExcessiveBufferOverrunPerHour = 2  # FATAL: Delta > 2/hour (per IBM Redbook)
LocalLinkIntegrityErrors = 0       # FATAL: Any increment (Delta > 0)
ReqTransportRetriesExceeded = 0    # FATAL: Any increment (Delta > 0) - Native IB only

#------------------------------------------------------------------------------
# Kernel Log Monitoring (NEW)
#------------------------------------------------------------------------------
[KernelLogMonitoring]
# Enable kernel log (dmesg) monitoring via /dev/kmsg
Enable = true

# Polling interval for checking new kernel messages
PollIntervalMs = 100

# Additional custom patterns (beyond built-in patterns)
# Comma-separated regex patterns
CustomPatterns = custom_driver_error,application_specific_pattern

#------------------------------------------------------------------------------
# Event Management (NEW)
#------------------------------------------------------------------------------
[EventManagement]
# Cooldown period to prevent duplicate events (seconds)
CooldownSeconds = 60


#------------------------------------------------------------------------------
# gRPC Settings (Existing)
#------------------------------------------------------------------------------
[gRPC]
Socket = unix:///var/run/nvsentinel.sock
MaxRetriesForRetryableError = 10
RetryDelaySecondsForRetryableError = 5
```

---

## 10. Event Management

### 10.1 Event Deduplication

Events are deduplicated using a cooldown window to prevent alert storms during hardware failures.

**Deduplication Logic:**

```
Event Key = DeviceName + Severity + CounterName

IF event key seen within cooldown period (default: 60s)
   --> Suppress event
ELSE
   --> Emit event and record timestamp
```

**Pruning:** Old entries are removed when they exceed 2× the cooldown period.

### 10.2 Event Routing via gRPC

All events are fatal and dispatched immediately.

**Routing Logic:**

```
1. Log event with [FATAL] prefix
2. Convert to protobuf HealthEvent
3. Immediate gRPC dispatch (5s timeout) to Platform Connector
```

**Routing Summary:**

| Event Type | Action |
|------------|--------|
| All events (FATAL) | Immediate gRPC dispatch to Platform Connector |

### 10.3 Event-to-HealthEvent Mapping (Protobuf)

The internal `NicHealthEvent` struct is designed to map directly to the platform connector's `pb.HealthEvent` protobuf schema.

**Field Mapping:**

| pb.HealthEvent Field | NicHealthEvent Field | Logic |
|----------------------|---------------------|-------|
| `Version` | `Version` | Protocol version (currently `1`) |
| `Agent` | `Agent` | Agent identifier (e.g., `"nic-health-monitor"`) |
| `CheckName` | `CheckName` | `"InfiniBandErrorCheck"` or `"EthernetErrorCheck"` based on LinkLayer |
| `ComponentClass` | `ComponentClass` | Always `"NIC"` |
| `GeneratedTimestamp` | `GeneratedTimestamp` | Event generation time via `timestamppb.New()` |
| `Message` | `Message` | Human-readable event description |
| `IsFatal` | `IsFatal` | Direct mapping (`true` = workload failure) |
| `IsHealthy` | `IsHealthy` | Direct mapping (`true` = healthy state) |
| `NodeName` | `NodeName` | Kubernetes node name from environment |
| `RecommendedAction` | `RecommendedAction` | `pb.RecommenedAction_NONE` (default) |
| `EntitiesImpacted` | `EntitiesImpacted` | Array of `{EntityType: "NIC", EntityValue: "<device_name>"}` |

**Example Event Construction:**

```go
event := &NicHealthEvent{
    Version:            1,
    Agent:              "nic-health-monitor",
    CheckName:          "InfiniBandErrorCheck",
    ComponentClass:     "NIC",
    GeneratedTimestamp: time.Now(),
    Message:            "Port mlx5_0 port 1: state DOWN - no connectivity",
    IsFatal:            true,
    IsHealthy:          false,
    NodeName:           os.Getenv("NODE_NAME"),
    RecommendedAction:  RecommendedAction_NONE,
    EntitiesImpacted:   []Entity{{EntityType: "NIC", EntityValue: "mlx5_0"}},
    // Internal fields
    NicType:            Infiniband,
    LinkLayer:          "InfiniBand",
}
```

## Appendix A: Quick Reference - Fatal Conditions Only

> **Reference**: [Linux Kernel sysfs-class-infiniband documentation](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)

### Fatal Counters

| Counter | Path | Threshold | Recommended Action | Source |
|---------|------|-----------|--------------------|--------|
| `symbol_error` | `counters/` | Delta > 120/hour | **RecommendedAction_REPLACE_VM** | [IBTA Spec](https://www.infinibandta.org/ibta-specification/) / [Oracle](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html) |
| `local_link_integrity_errors` | `counters/` | Delta > 0 | **RecommendedAction_REPLACE_VM** | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US) |
| `excessive_buffer_overrun_errors` | `counters/` | Delta > 2/hour | **RecommendedAction_REPLACE_VM** | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf) |
| `req_transport_retries_exceeded` | `hw_counters/` **(Native IB only)** | Delta > 0 | **RecommendedAction_REPLACE_VM** | [IBTA Spec](https://www.infinibandta.org/ibta-specification/) |

### Fatal States

| Condition | Recommended Action | Path/Source |
|-----------|--------------------|-------------|
| `state = DOWN` | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/state` |
| `phys_state = Disabled` | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` |
| `rate < target_rate` | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/rate` (Link Speed Degradation) |
| `operstate = down` | **RecommendedAction_REPLACE_VM** | `/sys/class/net/<dev>/operstate` |
| Device disappeared | **RecommendedAction_REPLACE_VM** | Device enumeration |
| PCI config = 0xFF | **RecommendedAction_REPLACE_VM** | PCI config space read |

### Fatal Kernel Log Patterns

| Pattern | Recommended Action | Meaning |
|---------|--------------------|---------|
| `mlx5_core.*ENABLE_HCA.*timeout` | **RecommendedAction_RESTART_BM** | NIC unusable |
| `mlx5_core.*health poll failed` | **RecommendedAction_REPLACE_VM** | NIC unhealthy |
| `mlx5_core.*unrecoverable` | **RecommendedAction_REPLACE_VM** | Hardware failure |
| `mlx5_core.*module.*absent` | **RecommendedAction_REPLACE_VM** | No transceiver |
| `PCIe.*fatal error` | **RecommendedAction_REPLACE_VM** | PCIe link broken |
| `NETDEV WATCHDOG.*transmit queue.*timed out` | **RecommendedAction_RESTART_BM** | TX stalled |

---








---

## References

### PHY & Signal Integrity
1. [PAM4 Error Correction Challenges in 400GbE (EDN)](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/)
2. [Determine Which Links Are Experiencing Significant Errors - Sun/Oracle (citing IBTA BER Threshold)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)

### Linux Kernel & Driver
3. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
4. [RHEL8 mlx5_core Stack Overflow (Red Hat)](https://access.redhat.com/solutions/6955682)

### Fabric Diagnostics
5. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)
6. [Black Hole Detection (sFlow)](https://blog.sflow.com/2016/05/black-hole-detection.html)
7. [InfiniBand™ Architecture Specification (IBTA)](https://www.infinibandta.org/ibta-specification/)

### Vendor Monitoring Guides
8. [InfiniBand Errors Dashboard - HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US)
9. [HPC Clusters Using InfiniBand on IBM Power Systems - IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)

---

