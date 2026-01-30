# NIC Health Monitor: Link State Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [State Monitoring Specification](#3-state-monitoring-specification)
4. [Link Speed Degradation Detection](#4-link-speed-degradation-detection)
5. [Device Discovery and Parsing](#5-device-discovery-and-parsing)
6. [State Change and Flap Detection](#6-state-change-and-flap-detection)
7. [PCI Configuration Space Health Check](#7-pci-configuration-space-health-check)
8. [SR-IOV Virtual Function Handling](#8-sr-iov-virtual-function-handling)
9. [RoCE State Monitoring](#9-roce-state-monitoring)
10. [Supported Hardware](#10-supported-hardware)
11. [Data Structures](#11-data-structures)
12. [Configuration](#12-configuration)
13. [Event Management](#13-event-management)
- [Appendix A: Quick Reference - Fatal State Conditions](#appendix-a-quick-reference---fatal-state-conditions)

**Related Documents:**
- [Link Counter Detection](./link-counter-detection.md) - Counter-based degradation monitoring
- [Syslog Detection & Correlation](./syslog-detection-correlation.md) - Kernel log monitoring and repeat failure detection

---

## 1. Overview

### 1.1 Problem Statement

Modern GPU clusters suffer from **Grey Failures** (subtle degradations) and **straggler effects** where a single degraded link throttles thousands of GPUs. Simple UP/DOWN polling is the first line of defense for detecting hard failures where the NIC becomes completely unavailable.

### 1.2 Scope of Link State Detection

This document covers the **State Monitoring** component of the NIC Health Monitor, which detects:

- **Hard UP/DOWN transitions** - Link completely lost, no connectivity
- **Device disappearance** - NIC no longer visible in sysfs (fell off PCIe bus)
- **Physical state changes** - Port disabled, polling, or in error recovery
- **Rate degradation** - Link renegotiated to lower speed (e.g., 400G → 200G)
- **Device disappearance** - Hardware failure detection

### 1.3 Binary Severity Model

This monitor uses a binary severity model based on **workload impact**:

| Severity      | Meaning                                  | Example                                            |
|---------------|------------------------------------------|----------------------------------------------------|
| **Fatal**     | Workload WILL fail or HAS failed         | NIC DOWN, device disappeared, phys_state=Disabled  |
| **Non-Fatal** | Degradation detected, workload continues | Transient state changes that recover automatically |

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

### 1.4 State Detection Overview Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     LINK STATE DETECTION FLOW                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                     DATA SOURCES (sysfs)                             │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │  /sys/class/infiniband/<dev>/ports/<port>/                          │   │
│  │  ├── state           →  Logical state (DOWN, INIT, ARMED, ACTIVE)   │   │
│  │  ├── phys_state      →  Physical state (LinkUp, Disabled, Polling)  │   │
│  │  └── rate            →  Negotiated link speed (e.g., 400 Gb/sec)    │   │
│  │                                                                      │   │
│  │  /sys/class/net/<interface>/                                         │   │
│  │  └── operstate       →  Interface state (up, down, unknown)         │   │
│  │                                                                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                  STATE MONITOR (1s polling interval)                 │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │                                                                      │   │
│  │  DETECTS:                                                            │   │
│  │  ├── Hard DOWN          → Link completely lost (FATAL)              │   │
│  │  ├── Device disappeared → NIC not in sysfs (FATAL)                  │   │
│  │  ├── Rate degradation   → Speed < expected (FATAL)                  │   │
│  │  ├── Physical disabled  → Port disabled (FATAL)                     │   │
│  │  └── Link error recovery → Active link problems (FATAL)             │   │
│  │                                                                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │              RAW EVENTS → PLATFORM CONNECTOR → MongoDB               │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │            HEALTH EVENTS ANALYZER (Correlation Rules)                │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │  • Link Flap Detection: "link_downed 3+ times in 10 min"            │   │
│  │  • Stabilization Windows: Prevent alert blinking                     │   │
│  │  • Cross-node correlation: Detect fabric-wide issues                 │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Architecture

### 2.1 Design Rationale: NVSentinel's "Report Raw, Correlate Centrally" Pattern

The State Monitor follows NVSentinel's established architectural pattern where:

1. **Health Monitors (DaemonSets)** report **raw events as-is** to the Platform Connector
2. **Health Events Analyzer (Centralized Deployment)** performs all correlation, aggregation, and pattern detection
3. **MongoDB** serves as the source of truth for event history and correlation queries

| Architectural Principle     | Implementation                             | Purpose                                                                   |
|-----------------------------|--------------------------------------------|---------------------------------------------------------------------------|
| **Raw Event Reporting**     | Each state change → immediate event        | Enables centralized correlation with full historical context              |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines   | Flexible, configurable rules without monitor code changes                 |
| **Temporal Correlation**    | Analyzer rules with time windows           | Detects patterns like "3 link flaps in 10 minutes"                        |
| **Stabilization Windows**   | Analyzer rules with sticky XID-style logic | Prevents "Alert Blinking" where transient recoveries hide critical issues |

### 2.2 Component Responsibilities

| Component                            | Responsibility                                                 | What It Does NOT Do                              |
|--------------------------------------|----------------------------------------------------------------|--------------------------------------------------|
| **NIC Health Monitor (State Check)** | Poll sysfs state files, detect UP/DOWN, send raw events        | Aggregation, deduplication, correlation, history |
| **Health Events Analyzer**           | Correlate events, detect link flap patterns, escalate severity | Direct hardware access                           |

### 2.3 State Check Data Flow (1s polling interval)

```
Reads:
├── state          → Logical link state (DOWN, INIT, ARMED, ACTIVE)
├── phys_state     → Physical layer state (LinkUp, Disabled, Polling, LinkErrorRecovery)
├── operstate      → Ethernet interface state (up, down, unknown)
└── rate           → Negotiated link speed (e.g., 100 Gb/sec, 200 Gb/sec)

Detects:
├── Hard DOWN      → Link completely lost, no connectivity
├── Device disappearance → NIC no longer visible in sysfs
├── Rate degradation → Link renegotiated to lower speed (e.g., 200G → 100G)
└── Physical disabled → Port administratively or physically disabled

On device disappearance:
└── Device not in sysfs → Hardware failure (FATAL)

Emits: Raw STATE_CHANGE events → Platform Connector → MongoDB
       (Link flap detection handled by Health Events Analyzer)
```

### 2.4 System Context

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                      NVSentinel NIC STATE MONITORING                           │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                       PER-NODE DAEMONSET                                 │  │
│  ├──────────────────────────────────────────────────────────────────────────┤  │
│  │                                                                          │  │
│  │  ┌────────────────────────────────────┐                                  │  │
│  │  │     NIC HEALTH MONITOR             │                                  │  │
│  │  │     (State Check - 1s interval)    │                                  │  │
│  │  │     ══════════════════════════     │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  DATA SOURCES:                     │                                  │  │
│  │  │  • /sys/class/infiniband/          │                                  │  │
│  │  │  • /sys/class/net/                 │                                  │  │
│  │  │  • /sys/bus/pci/devices/           │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  CHECKS:                           │                                  │  │
│  │  │  • InfiniBandStateCheck            │                                  │  │
│  │  │  • EthernetStateCheck              │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  BEHAVIOR:                         │                                  │  │
│  │  │  • Reports RAW state events        │                                  │  │
│  │  │  • NO aggregation                  │                                  │  │
│  │  │  • NO local persistence            │                                  │  │
│  │  └──────────────┬─────────────────────┘                                  │  │
│  │                 │                                                        │  │
│  └─────────────────┼────────────────────────────────────────────────────────┘  │
│                    │                                                           │
│                    ▼                                                           │
│  ┌──────────────────────────────────┐                                          │
│  │     PLATFORM CONNECTOR           │                                          │
│  │     ══════════════════           │                                          │
│  │  • Receives raw events           │                                          │
│  │  • Persists to MongoDB           │                                          │
│  │  • Triggers downstream           │                                          │
│  └──────────────┬───────────────────┘                                          │
│                 │                                                              │
│                 ▼                                                              │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                    HEALTH EVENTS ANALYZER                                │  │
│  │                    (Link Flap Detection)                                 │  │
│  │                    ══════════════════════                                │  │
│  │                                                                          │  │
│  │  NIC STATE CORRELATION RULES:                                            │  │
│  │  • RepeatedNICLinkFlap: "link_downed 3+ times in 10 min → REPLACE_VM"    │  │
│  │  • NICStabilizationWindow: Prevent flapping (similar to sticky XID)      │  │
│  └──────────────────────────────────────────────────────────────────────────┘  │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. State Monitoring Specification

### 3.1 Port States (Full Enumeration)

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

### 3.2 State Transitions

**Logical State Flow**: `DOWN (1)` → `INIT (2)` → `ARMED (3)` → `ACTIVE (4)`

- **DOWN**: No connectivity (FATAL)
- **INIT**: Initializing (problematic if stuck > 30s)
- **ARMED**: Waiting for Subnet Manager
- **ACTIVE**: Normal operational state (HEALTHY)

**Physical State Substates**: `Sleep (1)`, `Polling (2)`, `Disabled (3)`, `Training (4)`, `LinkUp (5)`, `LinkErrorRecovery (6)`

- **LinkErrorRecovery (6)**: Active error recovery in progress (FATAL)

### 3.3 Diagnostic Commands

```bash
# Check logical and physical port states
ibstat
# Output:
# CA 'mlx5_0'
# 	Port 1:
# 		State: Active
# 		Physical state: LinkUp
# 		Rate: 400
# 		Link layer: InfiniBand

# Check specific port state via sysfs
cat /sys/class/infiniband/mlx5_0/ports/1/state
# Output: 4: ACTIVE

cat /sys/class/infiniband/mlx5_0/ports/1/phys_state
# Output: 5: LinkUp

cat /sys/class/infiniband/mlx5_0/ports/1/rate
# Output: 400 Gb/sec (4X NDR)
```

### 3.4 State-Based Event Generation Algorithm

**Port State Evaluation Steps:**

1. **Read port state** from `/sys/class/infiniband/<dev>/ports/<port>/state`
2. **Evaluate logical state:**
   - If `state = DOWN` → Mark as **FATAL**, message: "Port DOWN - no connectivity"
   - If `state = INIT` → Warning, message: "Port stuck in INIT - check Subnet Manager"
   - If `state = ARMED` → Warning, message: "Port ARMED but not ACTIVE - check fabric manager"
   - If `state = ACTIVE` → Healthy

3. **Read physical state** from `/sys/class/infiniband/<dev>/ports/<port>/phys_state`
4. **Evaluate physical state:**
   - If `phys_state = Disabled` → Mark as **FATAL**, message: "Physical state DISABLED"
   - If `phys_state = Polling` → Warning, message: "Link training in progress"
   - If `phys_state = LinkErrorRecovery` → Mark as **FATAL**, message: "Active link problems"
   - If `phys_state = LinkUp` → Healthy

5. **Generate event** if any issues detected:
   - Set `IsFatal = true` for fatal conditions
   - Set `EntityType = "NICPort"`, `EntityValue = "<device>_port<n>"`
   - Set `RecommendedAction = REPLACE_VM` for fatal conditions

---

## 4. Link Speed Degradation Detection

### 4.1 The Problem

A common failure mode in GPU clusters is **link speed degradation**: a bad cable or transceiver causes the link to negotiate down (e.g., a 400Gbps NDR link training down to 200Gbps or 100Gbps). The link reports `State=Active` and `PhysState=LinkUp`, so standard state-based monitoring **will not fire**.

However, the workload (e.g., NCCL ring) will operate at a fraction of expected bandwidth, causing severe performance degradation for the entire cluster due to the slowest link bottleneck.

**Why This Happens:**
- PAM4 links (NDR/400G) have strict signal integrity requirements
- Degraded cables or dirty fiber connectors cause eye diagram closure
- Link Training State Machine (LTSM) automatically falls back to a lower speed to maintain connectivity
- The link "works" but at dramatically reduced throughput

### 4.2 Speed Degradation Detection Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      LINK SPEED DEGRADATION DETECTION                           │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  EXPECTED STATE (H100 Cluster):                                                  │
│  ┌─────────────────────────────────────────────────────────────────────┐        │
│  │  NIC: mlx5_0                                                         │        │
│  │  State: ACTIVE ✓     PhysState: LinkUp ✓     Rate: 400 Gb/s ✓       │        │
│  └─────────────────────────────────────────────────────────────────────┘        │
│                                                                                  │
│  DEGRADED STATE (Same H100 - Bad Cable):                                         │
│  ┌─────────────────────────────────────────────────────────────────────┐        │
│  │  NIC: mlx5_0                                                         │        │
│  │  State: ACTIVE ✓     PhysState: LinkUp ✓     Rate: 200 Gb/s ✗       │        │
│  │                                              └── 50% capacity loss!  │        │
│  └─────────────────────────────────────────────────────────────────────┘        │
│                                                                                  │
│  ═══════════════════════════════════════════════════════════════════════════    │
│                                                                                  │
│  IMPACT ON NCCL AllReduce (Ring Topology):                                       │
│                                                                                  │
│  ┌──────┐   400G   ┌──────┐   400G   ┌──────┐   200G   ┌──────┐                │
│  │ GPU0 │◄────────►│ GPU1 │◄────────►│ GPU2 │◄────────►│ GPU3 │                │
│  └──────┘          └──────┘          └──────┘    ↑     └──────┘                │
│                                                  │                              │
│                                           BOTTLENECK!                           │
│                                     Entire ring runs at 200G                    │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 4.3 Detection Mechanism

**Metric:** `/sys/class/infiniband/<dev>/ports/<port>/rate`

**Auto-Detection from GPU Product Name:**

Rather than requiring per-cluster configuration, the monitor infers the expected link speed from the **GPU product name**. This works because GPU clusters have standardized InfiniBand configurations based on the GPU generation.

**GPU-to-InfiniBand Mapping:**

| GPU Product | Expected Ports | Expected Rate  | Reference                                                                                          |
|-------------|----------------|----------------|----------------------------------------------------------------------------------------------------|
| A100        | 1              | 200 Gb/s       | [DGX A100 User Guide](https://docs.nvidia.com/dgx/dgxa100-user-guide/introduction-to-dgxa100.html) |
| H100        | 8              | 400 Gb/s (NDR) | [DGX H100 User Guide](https://docs.nvidia.com/dgx/dgxh100-user-guide/introduction-to-dgxh100.html) |
| H200        | 8              | 400 Gb/s (NDR) | NVIDIA Documentation                                                                               |
| B200        | 8              | 400 Gb/s       | [DGX B200 User Guide](https://docs.nvidia.com/dgx/dgxb200-user-guide/introduction-to-dgxb200.html) |
| GB200       | 8              | 400 Gb/s       | [GB200 NVL2](https://www.nvidia.com/en-us/data-center/gb200-nvl2/)                                 |

### 4.4 Speed Degradation Detection Algorithm

**GPU-to-Rate Lookup Steps:**

1. **Get GPU product name** via NVML or node labels (e.g., `nvidia.com/gpu.product`)
2. **Normalize** the product name to lowercase
3. **Match against `gpu_port_config`** entries using longest substring match
   - Example: "NVIDIA H100 80GB HBM3" matches "h100" → expects 8 ports at 400 Gb/s
4. **Return expected ports and rate** for the matched GPU type
5. If no match found, skip speed validation (unknown GPU type)

**Speed Validation Steps:**
1. Query GPU product name via NVML or from node labels (e.g., `nvidia.com/gpu.product`)
2. Look up expected port count and rate from `gpu_port_config` in the configuration file
3. Compare actual `rate` from sysfs against expected rate
4. If `actual_rate < expected_rate`, flag as degraded

**Extensibility:** To support future GPU generations, simply add new entries to the `gpu_port_config` section—no code changes required:
```yaml
gpu_port_config:
  x100:
    ports: 16
    rate_gbps: 800
```

> **Field Experience**: Link speed degradation is one of the most common undetected failure modes in GPU clusters. A single 200Gbps link in a 400Gbps fabric can reduce collective operation throughput by 50% because NCCL AllReduce operations are bounded by the slowest link.

---

## 5. Device Discovery and Parsing

### 5.1 Discovery Logic

The NIC Health Monitor discovers and parses InfiniBand/RoCE devices by iterating over sysfs:

1. Iterating over `/sys/class/infiniband`
2. Parsing `hca_type`, `fw_ver`, and `board_id`
3. Enumerating ports and reading `link_layer`, `state`, `phys_state`, and `rate`
4. Identifying device type (PF vs VF) for proper alerting

### 5.2 Device Discovery Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                        NIC DEVICE DISCOVERY FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  /sys/class/infiniband/                                                          │
│  │                                                                               │
│  ├── mlx5_0/                     ◄── Physical Function (PF)                      │
│  │   ├── hca_type                    → "MT4123" (ConnectX-6)                     │
│  │   ├── fw_ver                      → "20.31.1014"                              │
│  │   ├── board_id                    → "MT_0000000010"                           │
│  │   ├── device/                                                                 │
│  │   │   ├── sriov_totalvfs          → "16" (PF indicator)                       │
│  │   │   └── uevent                  → PCI_SLOT_NAME=0000:3b:00.0               │
│  │   └── ports/                                                                  │
│  │       └── 1/                                                                  │
│  │           ├── state               → "4: ACTIVE"                               │
│  │           ├── phys_state          → "5: LinkUp"                               │
│  │           ├── rate                → "400 Gb/sec (4X NDR)"                     │
│  │           ├── link_layer          → "InfiniBand"                              │
│  │           └── counters/           → (see counter detection doc)               │
│  │                                                                               │
│  ├── mlx5_1/ ... mlx5_17/        ◄── More Physical Functions                     │
│  │                                                                               │
│  └── mlx5_18/ ... mlx5_33/       ◄── Virtual Functions (VFs)                     │
│      └── device/                                                                 │
│          └── physfn → ../0000:3b:00.0  (VF indicator - points to parent PF)     │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 5.3 Vendor Detection

The monitor detects Mellanox devices using the following logic:
1. Check if device name matches `mlx5_\d+` (Mellanox).
2. Fallback: Check driver symlink in `/sys/class/infiniband/<dev>/device/driver` for `mlx5_core`.

| Vendor                 | Detection                              | State Monitoring  | Fatal Detection    |
|------------------------|----------------------------------------|-------------------|--------------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - state files | State + PCI checks |

---

## 6. State Change and Flap Detection

The NIC Health Monitor reports **raw state events** (each `link_downed` increment, each state change). The **Health Events Analyzer** performs pattern detection to distinguish between persistent drops and transient flapping.

### 6.1 Architecture

1. NIC Health Monitor reports each state change as a raw event
2. Raw events flow to MongoDB via Platform Connector
3. Health Events Analyzer applies correlation rules to detect patterns

### 6.2 Port Drop Detection (Analyzer Rule: `NICPortDrop`)

An InfiniBand port is marked as "Dropped" when the Analyzer detects:
- The port has been reporting `state=DOWN` for at least 4 minutes
- No `link_downed` delta events during this period (indicating no recovery attempts)

### 6.3 Port Flap Detection (Analyzer Rule: `RepeatedNICLinkFlap`)

An InfiniBand port is marked as "Flapping" (**Severity: FATAL**) when the Analyzer detects:
- 3+ `link_downed` events within 10 minutes on the same NICPort entity
- This indicates repeated DOWN→ACTIVE transitions (unstable hardware)

### 6.4 Link Flap Detection Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         LINK FLAP DETECTION FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  TIME        NIC STATE           RAW EVENTS SENT                                 │
│  ────        ─────────           ───────────────                                 │
│                                                                                  │
│  T+0:00      ACTIVE              (baseline)                                      │
│  T+1:30      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│  T+1:45      ACTIVE              (recovered)                                     │
│  T+4:20      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│  T+4:35      ACTIVE              (recovered)                                     │
│  T+7:10      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│              │                                                                   │
│              │   ┌─────────────────────────────────────────────────────────┐    │
│              └──►│        HEALTH EVENTS ANALYZER                           │    │
│                  │                                                          │    │
│                  │  Query: SELECT COUNT(*) FROM health_events              │    │
│                  │         WHERE agent = 'nic-health-monitor'              │    │
│                  │         AND message LIKE '%link_downed%'                │    │
│                  │         AND entity = 'mlx5_0_port1'                     │    │
│                  │         AND timestamp > NOW() - 10 minutes              │    │
│                  │                                                          │    │
│                  │  Result: 3 events                                        │    │
│                  │                                                          │    │
│                  │  Rule: RepeatedNICLinkFlap                               │    │
│                  │        IF count >= 3 THEN FATAL                         │    │
│                  │                                                          │    │
│                  │  ┌─────────────────────────────────────────────────┐    │    │
│                  │  │  OUTPUT: FATAL EVENT                            │    │    │
│                  │  │  Message: "NIC port flapping detected"          │    │    │
│                  │  │  RecommendedAction: REPLACE_VM                  │    │    │
│                  │  │  Entity: mlx5_0_port1                           │    │    │
│                  │  └─────────────────────────────────────────────────┘    │    │
│                  └─────────────────────────────────────────────────────────┘    │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

> **Effect**: The Analyzer emits a new fatal event with `RecommendedAction_REPLACE_VM`. The stabilization window logic (similar to sticky XID) can be implemented as an Analyzer rule to prevent rapid re-alerting.

---

## 7. Device Disappearance Handling

### 7.1 Purpose

When the State Monitor detects a device has disappeared from `/sys/class/infiniband/`, this is treated as a **FATAL** condition requiring VM replacement.

### 7.2 Detection

The monitor tracks devices across polling cycles. If a previously-seen device is no longer present in `/sys/class/infiniband/`, a FATAL event is generated immediately.

### 7.3 Event Classification

| Condition | Severity | Recommended Action |
|-----------|----------|-------------------|
| Device disappeared from `/sys/class/infiniband/` | **FATAL** | **RecommendedAction_REPLACE_VM** |

> **Design Note**: The current implementation does not differentiate between "clean" removals (driver unload) and "dirty" removals (hardware crash). All device disappearances are treated as FATAL because in production environments, unexpected device loss indicates a hardware issue requiring investigation and VM replacement.

---

## 8. SR-IOV Virtual Function Handling

### 8.1 Background: Why VFs Being DOWN is Expected

**SR-IOV (Single Root I/O Virtualization)** is a technology that allows a single physical NIC to appear as multiple virtual NICs. Understanding this is critical for correct alerting behavior.

> **Note**: Clusters with the **NVIDIA Network Operator** installed will have SR-IOV enabled by default.

**The Problem Without Understanding SR-IOV:**
```
Monitor starts → Sees 34 devices → 16 are DOWN → Generates 16 FATAL alerts!
But... those 16 devices are supposed to be DOWN. False alarm storm!
```

### 8.2 Key Terminology

| Term   | Full Name         | Description                                                     |
|--------|-------------------|-----------------------------------------------------------------|
| **PF** | Physical Function | The "real" NIC. Host OS controls it. Should ALWAYS be ACTIVE.   |
| **VF** | Virtual Function  | A "virtual clone" of the PF. Created for VMs/containers to use. |

### 8.3 VF Lifecycle

```
STAGE 1: System Boot
├── PF created: mlx5_0 → ACTIVE (host uses it)
├── VFs created: mlx5_18, mlx5_19, ... → DOWN (waiting for assignment)
└── This is NORMAL - VFs are like empty parking spots

STAGE 2: VM Starts
├── Orchestrator assigns VF to VM: mlx5_18 → VM1
├── mlx5_18 state changes: DOWN → ACTIVE
└── VM now has dedicated NIC hardware

STAGE 3: VM Shuts Down
├── VF released back to pool: mlx5_18
├── mlx5_18 state changes: ACTIVE → DOWN
└── Ready for next VM - back to "parking spot" state
```

### 8.4 Alerting Decision Matrix

| Device Type | State    | Should Alert? | Reason                                |
|-------------|----------|---------------|---------------------------------------|
| PF          | ACTIVE   | No            | Normal operation                      |
| PF          | **DOWN** | **YES!**      | Real problem - host lost connectivity |
| VF          | DOWN     | **No**        | Normal - VF not assigned to any VM    |
| VF          | ACTIVE   | No            | Normal - VF assigned and in use       |

### 8.5 Auto-Detection: PF vs VF

The Linux kernel provides clear indicators in sysfs:

| Indicator                    | PF (Physical Function)      | VF (Virtual Function)        |
|------------------------------|-----------------------------|------------------------------|
| `device/physfn` symlink      | Does NOT exist              | EXISTS (points to parent PF) |
| `device/sriov_totalvfs` file | EXISTS (shows max VF count) | Does NOT exist               |

```
# PF Example (mlx5_0):
/sys/class/infiniband/mlx5_0/device/
├── sriov_totalvfs    ← EXISTS (value: 16 = can create 16 VFs)
└── (no physfn)       ← Doesn't exist

# VF Example (mlx5_18):
/sys/class/infiniband/mlx5_18/device/
├── (no sriov_totalvfs)  ← Doesn't exist
└── physfn → ../0000:93:00.1  ← EXISTS (points to parent PF)
```

### 8.6 Real Example from Field Validation (34-NIC System)

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

### 8.7 Implementation

To determine if a `DOWN` state is expected, the monitor detects if a device is an SR-IOV Virtual Function (VF) or Physical Function (PF).

- **Method 1 (Primary)**: Check for `physfn` symlink in the device directory. If present, it's a VF.
- **Method 2 (Secondary)**: Check for `sriov_totalvfs` file. If present, it's a PF.

VFs are expected to be `DOWN` when unassigned. PFs are expected to be `ACTIVE`.

---

## 9. RoCE State Monitoring

RoCE (RDMA over Converged Ethernet) devices appear in **both** `/sys/class/net` and `/sys/class/infiniband`. The monitor accesses RoCE devices via the InfiniBand interface (`/sys/class/infiniband/`). The following monitoring applies to RoCE:

- **State monitoring**: `state`, `phys_state` (via InfiniBand sysfs interface)
- **Device identification**: Check `link_layer` file for "Ethernet"

### 9.1 GID Table Information (RoCE Routing Diagnostics)

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
- Empty GID table → `Error 61 (ENODATA)` during QP setup
- Missing IPv4 GIDs → routing issues for RoCE v2
- GID type mismatch between peers → connection failures

**Helper Functions:**
- `getGIDCount`: Enumerates `/sys/class/infiniband/<dev>/ports/<port>/gids/` to count valid GIDs.
- `getNetDevForIBDevice`: Discovers the network interface (e.g., `eth0`, `rdma4`) associated with an IB device by reading `/sys/class/infiniband/<dev>/device/net/`. This is critical for reading Ethernet statistics on RoCE devices.

---

## 10. Supported Hardware

> **Current Scope**: This initial implementation focuses on **Mellanox/NVIDIA InfiniBand and RoCE** devices only. The architecture is designed to be extensible for future support of additional NIC vendors.

| Vendor                 | Detection                              | State Monitoring  | Fatal Detection    |
|------------------------|----------------------------------------|-------------------|--------------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - state files | State + PCI checks |

### 10.1 Future Work

- **AWS EFA Support**: Device names matching `rdmap\d+s\d+`
- **Plain Ethernet**: `operstate = down` detection via `/sys/class/net/<interface>/operstate`
- **TCPXO Support**: TCP Express Offload support

---

## 11. Data Structures

### 11.1 State Monitoring Structures

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

### 11.2 Entity Model

NICs and Ports are modeled as separate entity types to enable precise fault localization:

| Entity Type | Entity Value Format | Example        | Use Case                                       |
|-------------|---------------------|----------------|------------------------------------------------|
| `NIC`       | `<device_name>`     | `mlx5_0`       | Device-level failures (disappeared, PCI error) |
| `NICPort`   | `<device>_port<n>`  | `mlx5_0_port1` | Port-level failures (DOWN, speed degradation)  |

**Rationale**: A single NIC (e.g., `mlx5_0`) can have multiple ports. Port-level failures should identify the specific port, enabling:
- Precise cable replacement (which port's cable is faulty)
- Targeted firmware diagnostics
- Accurate capacity planning (one failed port vs entire NIC)

---

## 12. Configuration

### 12.1 State Monitoring Configuration

```ini
#------------------------------------------------------------------------------
# General Settings
#------------------------------------------------------------------------------
[General]
# Polling interval for state monitoring
PollingIntervalInMilliseconds = 1000

# Maximum time to retry before confirming NIC is down
MaxRetryDurationForDownDetectedNICInMilliseconds = 500
RetryIntervalForDownDetectedNICInMilliseconds = 100

# Network type to monitor: "all", "infiniband", "roce"
MonitorNetworkType = all

# Regex patterns for NICs to exclude (comma-separated)
NicExclusionRegex = ^veth.*,^docker.*,^br-.*,^lo$

# Regex patterns for RoCE interface filtering
RoCEInterfaceRegex = ^rdma\d+$,^eth\d+$,^nic\d+\.\d+$

# sysfs paths (for containerized deployments with host mounts)
SysClassNetPath = /sys/class/net
SysClassInfinibandPath = /sys/class/infiniband

#------------------------------------------------------------------------------
# GPU-to-InfiniBand Port Configuration (Extensible)
#------------------------------------------------------------------------------
gpu_port_config:
  a100:
    ports: 1
    rate_gbps: 200

  h100:
    ports: 8
    rate_gbps: 400

  h200:
    ports: 8
    rate_gbps: 400

  b200:
    ports: 8
    rate_gbps: 400

  gb200:
    ports: 8
    rate_gbps: 400

#------------------------------------------------------------------------------
# SR-IOV Virtual Function Auto-Detection
#------------------------------------------------------------------------------
# VFs are expected to be DOWN when not assigned to a VM/container.
AutoDetectSRIOVVFs = true

# Manual override (OPTIONAL - only for non-SRIOV edge cases)
# ExpectedDownDevices = mlx5_disabled_port
# ExpectedDownDevicesRegex = ^mlx5_maintenance_.*$
```

---

## 13. Event Management

### 13.1 State Event Construction

Port state issues emit **Fatal** events with `RecommendedAction = REPLACE_VM`.

**Example Event Fields (Fatal - Port DOWN):**

| Field             | Value                                                    |
|-------------------|----------------------------------------------------------|
| Agent             | `nic-health-monitor`                                     |
| CheckName         | `InfiniBandErrorCheck`                                   |
| ComponentClass    | `NIC`                                                    |
| Message           | "Port mlx5_0 port 1: state DOWN - no connectivity"       |
| IsFatal           | `true`                                                   |
| IsHealthy         | `false`                                                  |
| RecommendedAction | `REPLACE_VM`                                             |
| EntitiesImpacted  | `[{EntityType: "NICPort", EntityValue: "mlx5_0_port1"}]` |

**Example Event Fields (Fatal - Device Disappeared):**

| Field             | Value                                                                     |
|-------------------|---------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                      |
| CheckName         | `InfiniBandErrorCheck`                                                    |
| ComponentClass    | `NIC`                                                                     |
| Message           | "NIC mlx5_0 disappeared from /sys/class/infiniband/ - hardware failure" |
| IsFatal           | `true`                                                                    |
| IsHealthy         | `false`                                                                   |
| RecommendedAction | `REPLACE_VM`                                                              |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`                            |

---

## Appendix A: Quick Reference - Fatal Condition Classification

The key question: **"Will the workload fail because of this?"**

### Fatal State Conditions (IsFatal = true)

| Condition                          | Recommended Action               | Rationale                                       |
|------------------------------------|----------------------------------|-------------------------------------------------|
| **NIC state = DOWN**               | **RecommendedAction_REPLACE_VM** | No network connectivity, workloads will timeout |
| **Device disappeared**             | **RecommendedAction_REPLACE_VM** | Hardware failure, immediate job failure         |
| **phys_state = Disabled**          | **RecommendedAction_REPLACE_VM** | Port disabled, no communication possible        |
| **phys_state = LinkErrorRecovery** | **RecommendedAction_REPLACE_VM** | Active link problems                            |
| **rate < target_rate**             | **RecommendedAction_REPLACE_VM** | Link speed degradation, collective bottleneck   |
| **Port flapping (3+ cycles)**      | **RecommendedAction_REPLACE_VM** | Intermittent hardware/cable instability         |

### Fatal Counters (IsFatal = true)

| Counter                           | Threshold           | Recommended Action               |
|-----------------------------------|---------------------|----------------------------------|
| `link_downed`                     | Delta > 0 (runtime) | **RecommendedAction_REPLACE_VM** |
| `excessive_buffer_overrun_errors` | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `local_link_integrity_errors`     | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `rnr_nak_retry_err`               | > 0 (any)           | **RecommendedAction_REPLACE_VM** |

### Fatal Driver/Firmware Logs (IsFatal = true)

Certain kernel logs indicate a deterministic hardware/driver failure:

| Pattern                | Recommended Action               | Rationale                                         |
|------------------------|----------------------------------|---------------------------------------------------|
| **cmd_exec timeout**   | **RecommendedAction_REPLACE_VM** | Control plane broken, driver cannot manage device |
| **health poll failed** | **RecommendedAction_REPLACE_VM** | Firmware heartbeat lost, device non-functional    |
| **unrecoverable**      | **RecommendedAction_REPLACE_VM** | Hardware admission of failure                     |
| **PCIe Fatal Error**   | **RecommendedAction_REPLACE_VM** | PCIe link broken, triggers system crash/reboot    |
| **NETDEV WATCHDOG**    | **RecommendedAction_REPLACE_VM** | Data path stalled, workload will fail             |

### Diagnostic Logs (IsFatal = false)

The following logs are **Warning (non-fatal)** and provide diagnostic context:

| Pattern              | IsFatal | Purpose                  |
|----------------------|---------|--------------------------|
| `insufficient power` | `false` | Power delivery context   |
| `High Temperature`   | `false` | Thermal warning          |
| `module absent`      | `false` | SFP/transceiver status   |
| `ACCESS_REG failed`  | `false` | Monitoring tool conflict |

> **Design Note**: Deterministically fatal events in logs trigger `REPLACE_VM`. Diagnostic logs remain as `Warning` for diagnostic context.

### State Detection Paths

| Condition                        | Recommended Action               | Path/Source                                           |
|----------------------------------|----------------------------------|-------------------------------------------------------|
| `state = DOWN`                   | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/state`      |
| `phys_state = Disabled`          | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` |
| `phys_state = LinkErrorRecovery` | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` |
| `rate < target_rate`             | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/rate`       |
| Device disappeared               | **RecommendedAction_REPLACE_VM** | Device enumeration in `/sys/class/infiniband/`        |

---

## References

1. [Linux Kernel sysfs-class-infiniband documentation](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
2. [DGX A100 User Guide](https://docs.nvidia.com/dgx/dgxa100-user-guide/introduction-to-dgxa100.html)
3. [DGX H100 User Guide](https://docs.nvidia.com/dgx/dgxh100-user-guide/introduction-to-dgxh100.html)
4. [DGX B200 User Guide](https://docs.nvidia.com/dgx/dgxb200-user-guide/introduction-to-dgxb200.html)
5. [GB200 NVL2](https://www.nvidia.com/en-us/data-center/gb200-nvl2/)

---
