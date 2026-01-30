# NIC Health Monitor: Link Counter Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Theoretical Foundation](#2-theoretical-foundation)
3. [Architecture](#3-architecture)
4. [Complete Counter Specification](#4-complete-counter-specification)
5. [Counter Reading and Parsing](#5-counter-reading-and-parsing)
6. [Counter Reset Handling](#6-counter-reset-handling)
7. [Missing Counter Handling](#7-missing-counter-handling)
8. [RDMA vs TCP/IP Counter Domains](#8-rdma-vs-tcpip-counter-domains)
9. [Data Structures](#9-data-structures)
10. [Configuration](#10-configuration)
11. [Event Management](#11-event-management)
- [Appendix A: Quick Reference - Counter Thresholds](#appendix-a-quick-reference---counter-thresholds)

**Related Documents:**
- [Link State Detection](./link-state-detection.md) - UP/DOWN state monitoring
- [Syslog Detection & Correlation](./syslog-detection-correlation.md) - Kernel log monitoring and repeat failure detection

---

## 1. Overview

### 1.1 Problem Statement

Modern GPU clusters suffer from **Grey Failures** (subtle degradations) and **straggler effects** where a single degraded link throttles thousands of GPUs. Simple UP/DOWN polling is insufficient; a deterministic degradation detection system is required that can detect both hard failures and **gradual degradation** before FEC exhaustion causes catastrophic packet loss.

### 1.2 Scope of Link Counter Detection

This document covers the **Degradation Monitoring** component of the NIC Health Monitor, which detects:

- **Fatal counter violations** - Counters that guarantee workload failure when incremented
- **Rate-based degradation** - Error rates exceeding thresholds that predict impending failure
- **Pre-failure prediction** - Detecting BER climbing before FEC exhaustion

### 1.3 Binary Severity Model

This monitor uses a binary severity model based on **workload impact**:

| Severity      | Meaning                                  | Example                                                      |
|---------------|------------------------------------------|--------------------------------------------------------------|
| **Fatal**     | Workload WILL fail or HAS failed         | `link_downed` (any), `excessive_buffer_overrun_errors` (any) |
| **Non-Fatal** | Degradation detected, workload continues | Symbol errors, congestion, link flapping                     |

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

### 1.4 Counter Detection Overview Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      LINK COUNTER DETECTION FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │                     DATA SOURCES (sysfs)                                 │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │  /sys/class/infiniband/<dev>/ports/<port>/                              │    │
│  │  ├── counters/                                                           │    │
│  │  │   ├── symbol_error                →  PHY bit errors (before FEC)     │    │
│  │  │   ├── link_error_recovery         →  Link retraining events          │    │
│  │  │   ├── link_downed                 →  Port training failures (FATAL)  │    │
│  │  │   ├── port_rcv_errors             →  Malformed packets               │    │
│  │  │   ├── local_link_integrity_errors →  Physical errors (FATAL)         │    │
│  │  │   ├── excessive_buffer_overrun    →  Lossless violation (FATAL)      │    │
│  │  │   └── port_xmit_discards          →  TX discards (congestion)        │    │
│  │  │                                                                       │    │
│  │  └── hw_counters/                    →  Extended counters               │    │
│  │      ├── roce_slow_restart           →  Victim flow oscillation         │    │
│  │      ├── local_ack_timeout_err       →  ACK timeout (path issues)       │    │
│  │      ├── rnr_nak_retry_err           →  Connection severed (FATAL)      │    │
│  │      └── req_transport_retries_exceeded → IB only (FATAL)               │    │
│  │                                                                          │    │
│  │  /sys/class/net/<interface>/statistics/                                  │    │
│  │  └── carrier_changes                 →  Link flap counter               │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │              DEGRADATION MONITOR (5s polling interval)                   │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │                                                                          │    │
│  │  CALCULATES (locally, for threshold comparison):                         │    │
│  │  ├── Δ (delta)      →  Change in counter value since last poll          │    │
│  │  └── Δ/Δt (rate)    →  Errors per second/minute/hour                    │    │
│  │                                                                          │    │
│  │  FATAL COUNTERS (immediate event):                                       │    │
│  │  ├── link_downed (Delta > 0)               →  FATAL                     │    │
│  │  ├── excessive_buffer_overrun (any)        →  FATAL                     │    │
│  │  ├── local_link_integrity_errors (any)     →  FATAL                     │    │
│  │  └── rnr_nak_retry_err (any)               →  FATAL                     │    │
│  │                                                                          │    │
│  │  NON-FATAL THRESHOLDS (degradation event):                               │    │
│  │  ├── symbol_error           > 10/sec       →  NON-FATAL                 │    │
│  │  ├── link_error_recovery    > 5/min        →  NON-FATAL                 │    │
│  │  ├── roce_slow_restart      > 10/sec       →  NON-FATAL                 │    │
│  │  └── carrier_changes        > 2/interval   →  NON-FATAL                 │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │              RAW EVENTS → PLATFORM CONNECTOR → MongoDB                   │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │            HEALTH EVENTS ANALYZER (Escalation Rules)                     │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │  • RepeatedNICDegradation: "5+ non-fatal events in 24h → FATAL"         │    │
│  │  • Pattern detection across time windows                                 │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Theoretical Foundation

### 2.1 The Physics of High-Speed Signaling Degradation

Modern interconnects (HDR/NDR InfiniBand, 100/200/400GbE) use **PAM4 modulation** (Pulse Amplitude Modulation, 4-level) to achieve high bandwidth. This represents a fundamental paradigm shift from previous generations.

#### 2.1.1 PAM4 vs NRZ: Why Velocity Monitoring is Required

| Aspect                  | NRZ (EDR/100GbE)   | PAM4 (NDR/400GbE)           |
|-------------------------|--------------------|-----------------------------|
| **Bits per symbol**     | 1                  | 2                           |
| **Voltage levels**      | 2 (0, 1)           | 4 (00, 01, 10, 11)          |
| **Eye height**          | Maximum            | **1/3 of NRZ**              |
| **SNR**                 | High               | **Drastically reduced**     |
| **Raw bit errors**      | Rare anomaly       | **Guaranteed and constant** |
| **Monitoring approach** | "Any error is bad" | **Velocity-based only**     |

> **Critical**: In PAM4 systems, raw bit errors are a **physical certainty**. A monitor that alerts on "Any Error > 0" would be permanently alarming. The velocity-based approach is the **only valid monitoring strategy** for 400G+ networks. ([Reference: PAM4 Test Challenges](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/))

### 2.2 Signal Degradation Progression

**Degradation flow**: Physical impairment (cable/SFP) → Eye diagram closes (DSP struggles) → Symbol errors (PHY layer) → FEC corrections (recoverable) → CRC failures (unrecoverable) → Packet loss (FATAL)

**Monitoring opportunity**: Detect degradation at the `symbol_error` stage, before FEC exhaustion causes packet loss.

### 2.3 Bit Error Rate (BER), FEC, and the "Cliff Effect"

Because errors are inevitable in PAM4, **Forward Error Correction (FEC) is mandatory** for 200G/400G/NDR links.

| Link Health State  | Bit Error Rate | Symbol Errors        | Action                 |
|--------------------|----------------|----------------------|------------------------|
| **Healthy**        | < 10E-15       | ~0 post-FEC          | None                   |
| **Failed (Fatal)** | > 10E-12       | FEC margin exhausted | **Fatal (REPLACE_VM)** |

#### 2.3.1 The FEC "Cliff Effect"

FEC masks physical degradation until the error rate exceeds correction capacity—then packet loss spikes instantly from 0% to ~100% (the "cliff"). The Degradation Monitor tracks **Pre-FEC BER** via `symbol_error` velocity, enabling node draining **before** the cliff is reached.

> **PAM4 Note (HDR/NDR)**: On 200G/400G adapters, non-zero raw BER is expected. Use rate-based thresholds (e.g., `symbol_error > 10/sec`) for degradation detection, not `symbol_error > 0`.

### 2.4 The Lossless Assumption and Deterministic Failure Horizons

Unlike general-purpose TCP/IP networks, which are architected to be resilient to packet loss, latency variation, and out-of-order delivery, RDMA fabrics—specifically InfiniBand (IB) and RDMA over Converged Ethernet (RoCE)—are designed under a **"lossless" assumption**. This architectural premise dictates that once a packet is admitted to the fabric, its delivery is guaranteed by credit-based flow control (in IB) or Priority Flow Control (in RoCE), relieving the transport layer of heavy congestion management overhead.

> **Key Insight**: This reliance on near-perfect transmission introduces a **binary fragility** to the system. When the physical or link layer violates the lossless assumption, the impact on the application is often not merely performance degradation, but **catastrophic failure**. For tightly coupled distributed workloads using MPI or NCCL, a failure in a single link **deterministically terminates the entire job**.

#### 2.4.1 Soft vs Hard Errors: The Determinism Boundary

The critical operational requirement is distinguishing between:

| Error Type      | Characteristics                            | Impact                                      |
|-----------------|--------------------------------------------|---------------------------------------------|
| **Soft Errors** | Probabilistic, recoverable via FEC/retries | Performance degradation, workload continues |
| **Hard Errors** | Deterministic, exceed recovery capacity    | Application failure **guaranteed**          |

The boundary between soft and hard errors is defined by:
1. **Counter thresholds** that indicate recovery mechanism exhaustion
2. **Rate of change** that exceeds retry bandwidth
3. **Specific counter types** that indicate fundamental violation of the lossless contract

#### 2.4.2 The 10E-12 BER Threshold

The InfiniBand specification defines a compliant link as maintaining a Bit Error Rate (BER) of better than **10E-12**. This physical constant provides the basis for threshold calculations:

- At a BER of 10E-12, a link running at high speed (e.g., HDR 200Gb/s) experiences a predictable number of errors per unit time
- **IBTA-compliant threshold**: Maximum allowable symbol error rate is **120 errors per hour** ([IBTA Specification](https://www.infinibandta.org/ibta-specification/) / [Oracle Documentation](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html))
- Below this rate, FEC algorithms can typically correct errors without retransmission
- Above this rate, the "effective" error rate (post-FEC) rises, leading to packet corruption and Link Level Retransmission (LLR) or transport layer retries

> **Monitoring Implication**: While a single `SymbolError` is not fatal, a rate exceeding **120/hour** (≈2/minute) is a **deterministic predictor of impending link instability**. Monitoring systems should treat this as a **Fatal condition** requiring node replacement.

#### 2.4.3 Deterministic Failure Mechanisms

The following counters represent **absolute deterministic failure** when they increment:

| Counter                             | Mechanism                                                | Why Deterministic                                                                       |
|-------------------------------------|----------------------------------------------------------|-----------------------------------------------------------------------------------------|
| **link_downed**                     | Port Training State Machine fails to maintain LinkUp     | Standard HPC applications do not support transparent dynamic rerouting of active QPs    |
| **excessive_buffer_overrun_errors** | HCA internal ingress buffer overflows                    | Violates fundamental "lossless" contract; packet causing overrun is dropped immediately |
| **RNR_nak_retry_err**               | Receiver Not Ready NAK retry exhausted                   | Terminal state of error handling; connection is severed                                 |
| **local_link_integrity_errors**     | Raw physical errors exceed LocalPhyErrors hardware limit | Link is operating outside design specifications                                         |

> **Note**: These four counters are the **only** counter-based fatal conditions. All other counters (symbol_error, port_rcv_errors, etc.) are non-fatal degradation indicators.

### 2.5 The Transport Layer Retry Window

When hardware counters increment, they don't directly cause application failure—they trigger a reaction in the software stack. Understanding this interaction defines the "Fatal" threshold:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    TRANSPORT LAYER RETRY WINDOW                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  Hardware: SymbolError ──► FEC fails ──► Packet Corrupted                       │
│                │                                                                 │
│                ▼                                                                 │
│  Receiver: drops packet ──► PortRcvErrors increments                            │
│                │                                                                 │
│                ▼                                                                 │
│  Sender: waits for ACK ──► Timeout ──► Retry (1) ──► ... ──► Retry (N)         │
│                │                                             │                   │
│                │                                             ▼                   │
│                │                                      GIVE UP                    │
│                │                                             │                   │
│                ▼                                             ▼                   │
│  Application: NCCL_IB_RETRY_CNT (default: 7) exhausted                          │
│                │                                                                 │
│                ▼                                                                 │
│  Result: QP transitions to ERROR state ──► Application crashes                  │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 2.6 Transport Retry Count Exceeded (Error 12)

When the NIC sends a packet and the ACK never arrives:

```
Send Packet → Wait for ACK → Timeout → Retry (1) → Timeout → ... → Retry (N) → GIVE UP
```

After `retry_cnt` attempts (default: 7), the NIC tears down the connection and the application receives `IBV_WC_RETRY_EXC_ERR`.

**Implications:**
- Confirms **Logical Link is broken** even if physical link is UP
- Often indicates "Silent Drop" or **Black Hole** in the fabric
- Local symptom of a **remote** problem

**What This Monitor CAN Detect**: The `local_ack_timeout_err` and `req_transport_retries_exceeded` (native IB) hardware counters track these retry events at the NIC level. Rising counter values indicate transport-layer problems even if we can't see the application error.

**Diagnostic Commands:**
```bash
# Read hardware counters directly from sysfs
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/req_transport_retries_exceeded
# Output: 42 (non-zero value indicates connection-severing retries)

# Detailed link quality and error counters via Mellanox diagnostic tools
mlxlink -d /dev/mst/mt4126_pciconf0 --show_ber
# Output: Symbol Errors, BER counters

# Query eye opening (signal quality indicator)
mlxlink -d /dev/mst/mt4126_pciconf0 --eye_open
# Output: Eye height/width for each PAM4 lane (identifies physical cable degradation)
```

**Correlation**: Use with [ibdiagnet](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf) to determine if issue is local (NIC) or remote (Switch/Fabric).

**Fabric-wide Diagnostic Command:**
```bash
# Perform comprehensive fabric-wide diagnostics (requires Subnet Manager access)
ibdiagnet -o /tmp/ibdiag_output
# Output: Summary of fabric errors, including symbol errors on switches and remote ports
```

---

## 3. Architecture

### 3.1 Design Rationale: NVSentinel's "Report Raw, Correlate Centrally" Pattern

The Degradation Monitor follows NVSentinel's established architectural pattern where:

1. **Health Monitors (DaemonSets)** report **raw events as-is** to the Platform Connector
2. **Health Events Analyzer (Centralized Deployment)** performs all correlation, aggregation, and pattern detection
3. **MongoDB** serves as the source of truth for event history and correlation queries

| Architectural Principle     | Implementation                             | Purpose                                                      |
|-----------------------------|--------------------------------------------|--------------------------------------------------------------|
| **Raw Event Reporting**     | Each threshold violation → immediate event | Enables centralized correlation with full historical context |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines   | Flexible, configurable rules without monitor code changes    |
| **Temporal Correlation**    | Analyzer rules with time windows           | Detects patterns like "5 degradation events in 24 hours"     |

### 3.2 Component Responsibilities

| Component                                  | Responsibility                                        | What It Does NOT Do                              |
|--------------------------------------------|-------------------------------------------------------|--------------------------------------------------|
| **NIC Health Monitor (Degradation Check)** | Poll sysfs counters, calculate rates, send raw events | Aggregation, deduplication, correlation, history |
| **Health Events Analyzer**                 | Correlate events, detect patterns, escalate severity  | Direct hardware access                           |

### 3.3 Degradation Check Data Flow (5s polling interval)

```
Reads:
├── counters/      → Standard IB counters (symbol_error, link_error_recovery, etc.)
├── hw_counters/   → Extended counters (roce_slow_restart, rnr_nak_retry_err, etc.)
├── statistics/    → Ethernet statistics (rx_crc_errors, rx_missed_errors, etc.)
└── carrier_changes → Link flap counter (catches UP/DOWN events between polls)

Calculates (locally, for threshold comparison):
├── Δ (delta)      → Change in counter value since last poll
└── Δ/Δt (rate)    → Errors per second (for threshold evaluation)

When threshold exceeded, emits RAW event with:
├── Counter name   → e.g., "symbol_error"
├── Current value  → e.g., 12500
├── Delta          → e.g., 150 (change since last poll)
├── Rate           → e.g., 30/sec
└── Threshold      → e.g., 10/sec

Fatal counter thresholds (configurable, defaults shown):
├── link_downed (Delta > 0)                    → QP disconnect (FATAL)
├── excessive_buffer_overrun_errors (any)      → Lossless violation (FATAL)
├── local_link_integrity_errors (any)          → Link outside spec (FATAL)
└── rnr_nak_retry_err (any)                    → Connection severed (FATAL)

Non-fatal thresholds (configurable, defaults shown):
├── symbol_error           > 10/sec
├── link_error_recovery    > 5/min
├── roce_slow_restart      > 10/sec
└── carrier_changes        > 2/interval

Emits: Raw DEGRADATION events → Platform Connector → MongoDB
       (Pattern detection and escalation handled by Health Events Analyzer)
```

---

## 4. Complete Counter Specification

### 4.1 Complete Counter Set ("Golden Counters" + Extended)

This monitor tracks both **fatal counters** (deterministic workload failure) and **non-fatal counters** (degradation indicators). The `IsFatal` field in the HealthEvent distinguishes between them.

#### 4.1.1 Standard Counters (`/sys/class/infiniband/<dev>/ports/<port>/counters/`)

| Counter                    | File Name                         | Degradation Meaning                                                                      | IsFatal | Alert Threshold                         | Source                                                                                                                                            |
|----------------------------|-----------------------------------|------------------------------------------------------------------------------------------|---------|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| **Symbol Error**           | `symbol_error`                    | Raw bit errors before FEC. Expected non-zero for PAM4 (HDR/NDR).                         | **No**  | Rate-based (e.g., > 10/sec for warning) | [Oracle/IBTA](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)                                                              |
| **Link Error Recovery**    | `link_error_recovery`             | PHY-initiated link retraining (micro-flapping). Causes millisecond-scale latency spikes. | **No**  | > 5/min (watchdog trigger)              | [Azure HPC](https://techcommunity.microsoft.com/blog/azurehighperformancecomputingblog/health-checks-for-hpc-workloads-on-microsoft-azure/837843) |
| **Link Downed**            | `link_downed`                     | Port Training State Machine failed to maintain LinkUp.                                   | **YES** | **Delta > 0 (Runtime)**                 | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                                                          |
| **Port Receive Errors**    | `port_rcv_errors`                 | Malformed packets (CRC, length errors). Saturates retry bandwidth at high rates.         | **No**  | > 10/sec (retry saturation)             | [NADDOD](https://www.naddod.com/blog/infiniband-symbol-error-counter)                                                                             |
| **Local Link Integrity**   | `local_link_integrity_errors`     | Raw physical errors exceeded LocalPhyErrors hardware cap. Link operating outside spec.   | **YES** | **> 0 (any)**                           | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                                                          |
| **Buffer Overrun**         | `excessive_buffer_overrun_errors` | HCA internal buffer overflow—**lossless contract violated**. Packet dropped immediately. | **YES** | **> 0 (any)**                           | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)                                                                           |
| **Port Transmit Discards** | `port_xmit_discards`              | TX discards due to congestion.                                                           | **No**  | > 100/sec                               |                                                                                                                                                   |

#### 4.1.2 Extended Counters (`/sys/class/infiniband/<dev>/ports/<port>/hw_counters/`) — Non-Fatal

All extended counters are **non-fatal** by default. They indicate congestion, retransmissions, or recoverable transport events. RDMA's reliable transport handles these automatically; workloads continue with potential performance impact.

**Key Non-Fatal Counters** (monitor for performance degradation):

| Category       | Counters                | IsFatal | Alert Threshold | Justification                                               |
|----------------|-------------------------|---------|-----------------|-------------------------------------------------------------|
| **Physical**   | `symbol_error`          | **No**  | > 10/sec        | PHY signal degradation / Dirty fiber.                       |
| **Link**       | `link_error_recovery`   | **No**  | > 5/min         | Link Flapping / PTSM Instability.                           |
| **Integrity**  | `port_rcv_errors`       | **No**  | > 10/sec        | FCS/CRC Corruption (Bit Rot).                               |
| **Congestion** | `port_xmit_discards`    | **No**  | > 100/sec       | Congestion Collapse / PFC breakdown.                        |
| **Transport**  | `roce_slow_restart`     | **No**  | > 10/sec        | Victim Flow / Transport Oscillation (Straggler).            |
| **Transport**  | `rnr_nak_retry_err`     | **YES** | **> 0 (any)**   | Receiver Not Ready NAK retry exhausted; connection severed. |
| **Timeout**    | `local_ack_timeout_err` | **No**  | > 1/sec         | Broken Path / Fabric Black Hole.                            |
| **Interface**  | `carrier_changes`       | **No**  | > 2/interval    | Physical instability visible to OS.                         |

> **Key Insights:**
> - **`rnr_nak_retry_err` > 0**: **FATAL** - Indicates RNR NAK retry exhausted; the connection has been severed.
> - **`roce_slow_restart` > 10/sec**: Primary indicator for Grey Failures. Indicates flow oscillation and straggler behavior.
> - **`port_xmit_discards` > 100/sec**: Flow control breakdown. Network physically unable to handle load.
> - **`symbol_error` > 10/sec**: Signature of "Dirty Fiber" or microscopic dust on connectors.

### 4.2 Counter Locations

- **Standard IB counters**: `/sys/class/infiniband/<dev>/ports/<port>/counters/` (symbol_error, link_downed, local_link_integrity_errors, etc.)
- **Extended counters (Mellanox)**: `/sys/class/infiniband/<dev>/ports/<port>/hw_counters/` (rnr_nak_retry_err, roce_slow_restart, etc.)
- **Ethernet stats (RoCE)**: `/sys/class/net/<iface>/statistics/` (carrier_changes)

### 4.3 Diagnostic Commands

```bash
# Read standard counters
cat /sys/class/infiniband/mlx5_0/ports/1/counters/symbol_error
cat /sys/class/infiniband/mlx5_0/ports/1/counters/port_rcv_errors
cat /sys/class/infiniband/mlx5_0/ports/1/counters/port_xmit_discards

# Read extended hw_counters (degradation monitoring)
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/local_ack_timeout_err
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/roce_slow_restart
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/rnr_nak_retry_err

# Fabric-wide diagnostics (requires Subnet Manager access)
ibdiagnet -o /tmp/ibdiag_output
```

### 4.4 Key Design Decisions

- **`link_downed`** is **Fatal**. In running MPI/NCCL jobs, any increment (Delta > 0) guarantees job crash.
- **`excessive_buffer_overrun_errors`** is **Fatal**. Violates fundamental "lossless" contract; packet causing overrun is dropped immediately.
- **`rnr_nak_retry_err`** is **Fatal**. Indicates Receiver Not Ready NAK retry exhausted; the connection has been severed.
- **`local_link_integrity_errors`** is **Fatal**. This counter is a "meta-threshold"—it only increments when raw physical errors exceed the hardware-defined LocalPhyErrors cap.
- **`symbol_error`** uses **PAM4 (HDR/NDR)** considerations. Zero-tolerance is obsolete for modern links; non-zero raw BER is expected. Monitor velocity for degradation trends.
- Most `hw_counters` are **Non-Fatal** by default—they indicate degradation that should be monitored but doesn't immediately crash workloads. Exception: `rnr_nak_retry_err` is fatal.

### 4.5 Consolidated Deterministic Failure Thresholds (Defaults)

> **Configuration Note**: All thresholds and severity levels are configurable. The values below are **defaults** based on industry specifications and vendor recommendations. See [Section 10: Configuration](#10-configuration) for customization options.

**Table 1: Absolute Deterministic Failure Thresholds (Default: Fatal - IsFatal=true)**

Breaching these thresholds **guarantees application failure** or mandatory node exclusion.

| Counter Name                      | Type     | Fatal Threshold         | IsFatal | Deterministic Mechanism                                                                            | Source                                                                                   |
|-----------------------------------|----------|-------------------------|---------|----------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------|
| `link_downed`                     | Standard | **Delta > 0 (Runtime)** | **YES** | Logical path destruction; QP disconnect. Standard HPC apps don't support transparent QP rerouting. | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us) |
| `excessive_buffer_overrun_errors` | Standard | **> 0 (Any)**           | **YES** | Lossless guarantee violation; packet dropped immediately. HCA ingress buffer overflow.             | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)                  |
| `rnr_nak_retry_err`               | Extended | **> 0 (Any)**           | **YES** | Receiver Not Ready NAK retry exhausted; connection severed. Terminal state of error handling.      | [Datadog IB](https://docs.datadoghq.com/integrations/infiniband/)                        |
| `local_link_integrity_errors`     | Standard | **> 0 (Any)**           | **YES** | Physical error density exceeds hardware-defined LocalPhyErrors cap. Link outside spec.             | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us) |

**Table 2: Predictive Thresholds (Non-Fatal - IsFatal=false)**

Breaching these rates indicates **degradation** requiring monitoring. Workloads continue but performance may be impacted.

| Counter Name            | Type      | Alert Threshold  | IsFatal | Rationale                                                            | Source                                                                                                                                            |
|-------------------------|-----------|------------------|---------|----------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `symbol_error`          | PHY       | **> 10/sec**     | **No**  | Physical layer degradation (Dirty Fiber).                            | [Oracle/IBTA](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)                                                              |
| `link_error_recovery`   | Link      | **> 5/min**      | **No**  | PTSM Instability. Latency accumulation triggers watchdog timeouts.   | [Azure HPC](https://techcommunity.microsoft.com/blog/azurehighperformancecomputingblog/health-checks-for-hpc-workloads-on-microsoft-azure/837843) |
| `port_rcv_errors`       | Standard  | **> 10/sec**     | **No**  | Bit Rot / CRC Corruption. Saturation of transport replay buffer.     | [NADDOD](https://www.naddod.com/blog/infiniband-symbol-error-counter)                                                                             |
| `port_xmit_discards`    | Standard  | **> 100/sec**    | **No**  | Congestion Collapse / PFC breakdown.                                 |                                                                                                                                                   |
| `roce_slow_restart`     | RoCE      | **> 10/sec**     | **No**  | "Victim Flow" oscillation. Jitter impacts AllReduce synchronization. | [NVIDIA DOCA](https://docs.nvidia.com/doca/archive/2-9-3/doca+telemetry+service+guide/index.html)                                                 |
| `local_ack_timeout_err` | Transport | **> 1/sec**      | **No**  | ACK timeouts indicate path issues (Black Hole).                      |                                                                                                                                                   |
| `carrier_changes`       | Interface | **> 2/interval** | **No**  | Link instability (catches UP/DOWN events between polls).             |                                                                                                                                                   |

### 4.6 Technical Justification for Non-Fatal Thresholds

The following analysis validates the efficacy of the proposed monitoring design based on hardware specifications and empirical reliability studies.

**1. Physical Layer (L1) Justifications**
*   **Symbol Error (`symbol_error` > 10/sec)**: A rate of 10/sec is a robust indicator of physical degradation. In modern PAM4 links, a healthy optical connection operates with a BER better than 1E-12 (roughly one error every few hours). A rate of 10/sec implies the BER has degraded by orders of magnitude (to ~1E-8). This is the classic signature of "Dirty Fiber" or microscopic dust on connectors.
*   **Link Error Recovery (`link_error_recovery` > 5/min)**: Tracks the Port Training State Machine (PTSM). 5 events per minute represents a "Flapping" link. While the link recovers (non-fatal), each retrain causes 50ms to 2s of stall, decimating performance for synchronous GPU workloads.
*   **Carrier Changes (`carrier_changes` > 2/interval)**: The OS-visible shadow of link recovery. Confirms that physical instability was severe enough to disrupt the driver layer.

**2. Data Link Layer (L2) Justifications**
*   **Port Receive Errors (`port_rcv_errors` > 10/sec)**: Indicates "Bit Rot"—data corruption surviving the PHY but failing the CRC/FCS check. Triggers "Phantom Congestion" as the network repeatedly retransmits corrupted frames.
*   **Port Transmit Discards (`port_xmit_discards` > 100/sec)**: Indicates flow control breakdown. The network is physically unable to handle the load, and backpressure mechanisms (PFC) are failing. Definitive signal of Congestion Collapse.

**3. Transport Layer (L4) Justifications**
*   **RoCE Slow Restart (`roce_slow_restart` > 10/sec)**: Primary indicator for Grey Failures. Indicates a flow is timing out and resetting its congestion window repeatedly. This creates stragglers that stall the entire GPU fleet during collective operations (AllReduce).
*   **Local ACK Timeout (`local_ack_timeout_err` > 1/sec)**: In a reliable lossless network, ACKs should not be lost. A persistent rate of 1/sec implies a "Fabric Black Hole" (e.g., a specific bad ECMP path).

> **Note on `rnr_nak_retry_err`**: This counter is **FATAL** (not a non-fatal threshold). Any increment indicates the Receiver Not Ready NAK retry limit has been exhausted and the connection has been severed. This is a terminal state of error handling.

> **Final Verdict**: These thresholds are calibrated to distinguish between background noise (standard FEC activity) and pathological hardware degradation that threatens AI training efficiency.

---

## 5. Counter Reading and Parsing

### 5.1 Mellanox Counter Reading

For Mellanox devices (IB and RoCE), the monitor reads:
1.  **Standard Counters**: `/sys/class/infiniband/<dev>/ports/1/counters/`
    *   Fatal counters: `symbol_error`, `link_downed`, `local_link_integrity_errors`, `excessive_buffer_overrun_errors`, `req_transport_retries_exceeded` (Native IB)
2.  **Extended Counters**: `/sys/class/infiniband/<dev>/ports/1/hw_counters/`
    *   Non-fatal counters for degradation monitoring

> **Note**: Mellanox throughput counters (`port_rcv_data`, `port_xmit_data`) are in 4-byte words. Multiply by 4 to get bytes.

### 5.2 Mellanox Fatal Counter Paths

| Counter                           | Path                                                                                  | Fatal Threshold                |
|-----------------------------------|---------------------------------------------------------------------------------------|--------------------------------|
| `symbol_error`                    | `/sys/class/infiniband/<dev>/ports/<port>/counters/symbol_error`                      | Delta > 120/hour               |
| `local_link_integrity_errors`     | `/sys/class/infiniband/<dev>/ports/<port>/counters/local_link_integrity_errors`       | Delta > 0                      |
| `excessive_buffer_overrun_errors` | `/sys/class/infiniband/<dev>/ports/<port>/counters/excessive_buffer_overrun_errors`   | Delta > 2/hour                 |
| `req_transport_retries_exceeded`  | `/sys/class/infiniband/<dev>/ports/<port>/hw_counters/req_transport_retries_exceeded` | Delta > 0 **(Native IB only)** |

> **Note**: The `symbol_error > 120/hour` threshold is per [IBTA specification (10E-12 BER)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html). `req_transport_retries_exceeded` is only available on Native InfiniBand (not RoCE).

---

## 6. Counter Reset Handling

Hardware counters may reset due to driver reloads, device resets, or (rarely) uint64 overflow. The monitor must handle cases where `Current < Previous` to avoid incorrect delta calculations.

### 6.1 The Problem

```
Poll N:   symbol_error = 1,000,000
Driver Reload / Counter Reset
Poll N+1: symbol_error = 50
Naive Delta = 50 - 1,000,000 = NEGATIVE (or overflow to huge positive)
```

### 6.2 Counter Reset Handling Algorithm

**Delta Calculation Steps:**

1. **Compare current vs previous** counter value
2. **If current < previous** (reset detected):
   - Treat the new value as the delta since the reset
   - Return `current` as the delta
3. **Otherwise**:
   - Return `current - previous` as the delta

**Counter Evaluation Steps:**

1. **Calculate delta** using the algorithm above
2. **Convert to hourly rate**: `hourlyRate = delta × (3600 / pollingIntervalSeconds)`
3. **Compare against threshold**:
   - If `hourlyRate > thresholdPerHour` → Generate FATAL event
   - Otherwise → No event (counter within acceptable range)
4. **For fatal counters** (link_downed, excessive_buffer_overrun, etc.):
   - Any delta > 0 triggers immediate FATAL event

### 6.3 Rationale

- When a counter resets, the new value represents errors accumulated since the reset
- This is a conservative approach: we may slightly undercount errors immediately after a reset
- Alternative (treating reset as zero delta) could miss real errors that occurred during/after reset
- Driver reloads are logged separately by the Syslog Health Monitor, providing correlation context

---

## 7. Missing Counter Handling

Not all counters are available on all NIC versions or firmware revisions. The monitor must gracefully handle missing counters to ensure portability across different hardware generations (ConnectX-5, ConnectX-6, ConnectX-7, etc.).

### 7.1 Design Principles

- **Fail-open for missing counters**: If a counter file does not exist, skip it silently. Do not emit errors or events.
- **Log at startup only**: On monitor initialization, log which counters are available vs. unavailable for debugging purposes. Do not repeatedly log missing counters during polling.
- **Graceful degradation**: The monitor should function with whatever subset of counters is available. A node with an older NIC still benefits from the counters that do exist.
- **Configuration flexibility**: Allow operators to disable specific counters via configuration if they are known to be unavailable or irrelevant for their environment.

### 7.2 Common Counter Availability by NIC Generation

| Counter                 | ConnectX-5 | ConnectX-6 | ConnectX-7 |
|-------------------------|------------|------------|------------|
| `symbol_error`          | Yes        | Yes        | Yes        |
| `link_error_recovery`   | Yes        | Yes        | Yes        |
| `link_downed`           | Yes        | Yes        | Yes        |
| `port_rcv_errors`       | Yes        | Yes        | Yes        |
| `roce_slow_restart`     | No         | Yes        | Yes        |
| `local_ack_timeout_err` | Yes        | Yes        | Yes        |

> **Note**: Counter availability may also depend on firmware version and driver configuration. The monitor should always verify counter existence at runtime rather than relying on static assumptions.

---

## 8. RDMA vs TCP/IP Counter Domains

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
> RDMA-layer issues like `roce_slow_restart` errors.

### 8.1 Counter Domain Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    RDMA vs TCP/IP COUNTER DOMAINS                                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│                         ┌─────────────────────────────┐                         │
│                         │      APPLICATION LAYER      │                         │
│                         └──────────────┬──────────────┘                         │
│                                        │                                         │
│                    ┌───────────────────┼───────────────────┐                    │
│                    │                   │                   │                    │
│                    ▼                   │                   ▼                    │
│  ┌─────────────────────────┐           │     ┌─────────────────────────┐        │
│  │      RDMA STACK         │           │     │      TCP/IP STACK       │        │
│  │  (NCCL, MPI, ib_*)      │           │     │  (HTTP, SSH, ping)      │        │
│  └───────────┬─────────────┘           │     └───────────┬─────────────┘        │
│              │                         │                 │                      │
│              ▼                         │                 ▼                      │
│  ┌─────────────────────────┐           │     ┌─────────────────────────┐        │
│  │ InfiniBand Counters     │           │     │ Ethernet Statistics     │        │
│  │ /sys/class/infiniband/  │           │     │ /sys/class/net/         │        │
│  │ <dev>/ports/<p>/counters│           │     │ <iface>/statistics/     │        │
│  │                         │           │     │                         │        │
│  │ • symbol_error          │           │     │ • rx_bytes              │        │
│  │ • port_rcv_errors       │           │     │ • tx_bytes              │        │
│  │ • roce_slow_restart     │           │     │ • rx_errors             │        │
│  │ • port_rcv_data         │           │     │ • carrier_changes       │        │
│  └───────────┬─────────────┘           │     └───────────┬─────────────┘        │
│              │                         │                 │                      │
│              └─────────────────────────┴─────────────────┘                      │
│                                        │                                         │
│                                        ▼                                         │
│                         ┌─────────────────────────────┐                         │
│                         │     PHYSICAL NIC HARDWARE   │                         │
│                         │        (ConnectX-6)         │                         │
│                         └─────────────────────────────┘                         │
│                                                                                  │
│  ═══════════════════════════════════════════════════════════════════════════    │
│                                                                                  │
│  KEY INSIGHT: Monitor InfiniBand counters for RDMA workload health              │
│               Ethernet stats won't catch roce_slow_restart!                      │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 9. Data Structures

### 9.1 Counter Structures

```go
// CounterSnapshot represents a point-in-time reading of all counters for a port
type CounterSnapshot struct {
    Device    string             `json:"device"`
    Port      int                `json:"port"`
    Timestamp time.Time          `json:"timestamp"`
    Counters  map[string]uint64  `json:"counters"`  // counter_name -> value
}

// CounterDelta represents the change between two snapshots
type CounterDelta struct {
    Device       string             `json:"device"`
    Port         int                `json:"port"`
    IntervalSec  float64            `json:"interval_sec"`
    Deltas       map[string]uint64  `json:"deltas"`       // counter_name -> delta
    Rates        map[string]float64 `json:"rates"`        // counter_name -> rate/sec
}
```

### 9.2 Entity Model

NICs and Ports are modeled as separate entity types to enable precise fault localization:

| Entity Type | Entity Value Format | Example        | Use Case                      |
|-------------|---------------------|----------------|-------------------------------|
| `NIC`       | `<device_name>`     | `mlx5_0`       | Device-level failures         |
| `NICPort`   | `<device>_port<n>`  | `mlx5_0_port1` | Port-level counter violations |

---

## 10. Configuration

The counter monitoring system is fully configurable, allowing operators to:
- Define which counters to monitor
- Configure threshold types (delta-based or velocity-based)
- Set fatal/non-fatal severity levels per counter
- Override default thresholds for specific environments

### 10.1 Configuration Schema

```yaml
# NIC Health Monitor - Counter Detection Configuration
counterDetection:
  # Enable/disable counter monitoring
  enabled: true
  
  # Polling interval for counter collection (milliseconds)
  pollIntervalMs: 5000
  
  # Counter definitions - fully configurable
  # Each counter can specify:
  #   - name:           Counter identifier (used in events)
  #   - path:           Sysfs path relative to device (supports standard and hw_counters)
  #   - enabled:        Enable/disable this counter (default: true)
  #   - isFatal:        Whether threshold breach triggers Fatal event (default: false)
  #   - thresholdType:  "delta" (absolute change) or "velocity" (rate per time unit)
  #   - threshold:      Numeric threshold value
  #   - velocityUnit:   For velocity thresholds: "per_second", "per_minute", "per_hour"
  #   - description:    Human-readable description for event messages
  #   - recommendedAction: Action for fatal counters (REPLACE_VM, RESTART_BM, NONE)
  
  counters: []  # See defaults below
```

### 10.2 Default Counter Configuration

The following counters are monitored by default. Operators can override any setting or add custom counters.

```yaml
counterDetection:
  enabled: true
  pollIntervalMs: 5000
  
  counters:
    #--------------------------------------------------------------------------
    # FATAL COUNTERS (Default: IsFatal=true, RecommendedAction=REPLACE_VM)
    # These counters indicate deterministic workload failure
    #--------------------------------------------------------------------------
    
    - name: link_downed
      path: counters/link_downed
      enabled: true
      isFatal: true
      thresholdType: delta
      threshold: 0              # Any increment (> 0) is fatal
      description: "Port Training State Machine failed - QP disconnect"
      recommendedAction: REPLACE_VM
      
    - name: excessive_buffer_overrun_errors
      path: counters/excessive_buffer_overrun_errors
      enabled: true
      isFatal: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
      description: "HCA internal buffer overflow - lossless contract violated"
      recommendedAction: REPLACE_VM
      
    - name: local_link_integrity_errors
      path: counters/local_link_integrity_errors
      enabled: true
      isFatal: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
      description: "Physical errors exceed LocalPhyErrors hardware cap"
      recommendedAction: REPLACE_VM
      
    - name: rnr_nak_retry_err
      path: hw_counters/rnr_nak_retry_err
      enabled: true
      isFatal: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
      description: "Receiver Not Ready NAK retry exhausted - connection severed"
      recommendedAction: REPLACE_VM
    
    #--------------------------------------------------------------------------
    # NON-FATAL COUNTERS (Default: IsFatal=false, RecommendedAction=NONE)
    # These counters indicate degradation requiring monitoring
    #--------------------------------------------------------------------------
    
    # Physical Layer (PHY)
    - name: symbol_error
      path: counters/symbol_error
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: per_second
      description: "PHY bit errors before FEC - physical layer degradation"
      recommendedAction: NONE
      
    - name: link_error_recovery
      path: counters/link_error_recovery
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 5.0
      velocityUnit: per_minute
      description: "Link retraining events - micro-flapping"
      recommendedAction: NONE
    
    # Transport Layer
    - name: port_rcv_errors
      path: counters/port_rcv_errors
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: per_second
      description: "Malformed packets received"
      recommendedAction: NONE
      
    - name: out_of_sequence
      path: hw_counters/out_of_sequence
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 100.0
      velocityUnit: per_second
      description: "Fabric routing issues - out of sequence packets"
      recommendedAction: NONE
      
    - name: local_ack_timeout_err
      path: hw_counters/local_ack_timeout_err
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 1.0
      velocityUnit: per_second
      description: "ACK timeout - potential fabric black hole"
      recommendedAction: NONE
    
    # Congestion Indicators
    - name: port_xmit_discards
      path: counters/port_xmit_discards
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 100.0
      velocityUnit: per_second
      description: "TX discards due to congestion"
      recommendedAction: NONE
      
    - name: port_xmit_wait
      path: counters/port_xmit_wait
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 10000.0
      velocityUnit: per_second
      description: "TX wait ticks - congestion backpressure"
      recommendedAction: NONE
    
    # RoCE-specific
    - name: roce_slow_restart
      path: hw_counters/roce_slow_restart
      enabled: true
      isFatal: false
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: per_second
      description: "Victim flow oscillation"
      recommendedAction: NONE
    
    # Interface Level
    - name: carrier_changes
      path: /sys/class/net/{interface}/statistics/carrier_changes
      enabled: true
      isFatal: false
      thresholdType: delta
      threshold: 2               # > 2 changes per interval
      description: "Link instability - carrier state changes"
      recommendedAction: NONE
```

### 10.3 Custom Counter Example

Operators can add custom counters or override defaults:

```yaml
counterDetection:
  counters:
    # Override: Make symbol_error fatal for strict environments
    - name: symbol_error
      path: counters/symbol_error
      enabled: true
      isFatal: true                    # Override: make fatal
      thresholdType: velocity
      threshold: 120.0                 # IBTA spec: 120/hour
      velocityUnit: per_hour           # Changed from per_second
      description: "Symbol errors exceed IBTA BER threshold"
      recommendedAction: REPLACE_VM
    
    # Custom: Add vendor-specific counter
    - name: custom_vendor_error
      path: hw_counters/vendor_specific_err
      enabled: true
      isFatal: false
      thresholdType: delta
      threshold: 100
      description: "Vendor-specific error counter"
      recommendedAction: NONE
    
    # Disable: Turn off a default counter
    - name: port_xmit_wait
      enabled: false
```

### 10.4 Threshold Processing Algorithm

```
For each configured counter:
  1. Read current value from sysfs path
  2. Calculate delta: current_value - previous_value
  3. Handle counter reset if delta < 0 (treat as delta = current_value)
  
  4. Based on thresholdType:
     
     IF thresholdType == "delta":
       breach = (delta > threshold)
     
     IF thresholdType == "velocity":
       Calculate rate based on velocityUnit:
         - per_second: rate = delta / (pollIntervalMs / 1000)
         - per_minute: rate = delta / (pollIntervalMs / 60000)
         - per_hour:   rate = delta / (pollIntervalMs / 3600000)
       breach = (rate > threshold)
  
  5. If breach:
     Generate HealthEvent:
       - IsFatal = counter.isFatal
       - RecommendedAction = counter.recommendedAction
       - Message = counter.description + " (value={value}, delta={delta}, rate={rate})"
       - CheckName = "CounterThresholdCheck"
       - ComponentClass = "NIC"
```

### 10.5 Configuration Validation

The monitor validates configuration at startup:

| Validation            | Requirement                                       | Action on Failure         |
|-----------------------|---------------------------------------------------|---------------------------|
| Counter path exists   | Path must be readable in sysfs                    | Log warning, skip counter |
| Threshold is positive | threshold >= 0                                    | Reject configuration      |
| velocityUnit valid    | Must be `per_second`, `per_minute`, or `per_hour` | Reject configuration      |
| thresholdType valid   | Must be `delta` or `velocity`                     | Reject configuration      |
| Unique counter names  | No duplicate `name` fields                        | Reject configuration      |

---

## 11. Event Management

### 11.1 Event Construction

**Example Event Fields (Fatal - link_downed):**

| Field             | Value                                                                           |
|-------------------|---------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                            |
| CheckName         | `InfiniBandErrorCheck`                                                          |
| ComponentClass    | `NIC`                                                                           |
| Message           | "Port mlx5_0 port 1: link_downed counter incremented (Delta=1) - QP disconnect" |
| IsFatal           | `true`                                                                          |
| IsHealthy         | `false`                                                                         |
| RecommendedAction | `REPLACE_VM`                                                                    |
| EntitiesImpacted  | `[{EntityType: "NICPort", EntityValue: "mlx5_0_port1"}]`                        |

**Example Event Fields (Non-Fatal - Degradation Warning):**

| Field             | Value                                                       |
|-------------------|-------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                        |
| CheckName         | `InfiniBandErrorCheck`                                      |
| ComponentClass    | `NIC`                                                       |
| Message           | "Port mlx5_0 port 1: symbol_error rate elevated (15.2/sec)" |
| IsFatal           | `false`                                                     |
| IsHealthy         | `false`                                                     |
| RecommendedAction | `NONE` (monitor for escalation)                             |
| EntitiesImpacted  | `[{EntityType: "NICPort", EntityValue: "mlx5_0_port1"}]`    |

### 11.2 Event Routing

| IsFatal | Action                                        | Use Case                    |
|---------|-----------------------------------------------|-----------------------------|
| `true`  | Immediate gRPC dispatch to Platform Connector | link_downed, buffer overrun |
| `false` | Batched gRPC dispatch (periodic)              | Symbol errors, congestion   |

---

## Appendix A: Quick Reference - Default Counter Thresholds

> **Note**: All counters, thresholds, and severity levels are configurable via the monitor configuration. The values below are the **defaults** that apply when no custom configuration is provided. See [Section 10: Configuration](#10-configuration) for customization options.

### Fatal Counters (Default: IsFatal = true)

| Counter                           | Path           | Default Threshold | Default Action | Configurable |
|-----------------------------------|----------------|-------------------|----------------|--------------|
| `link_downed`                     | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `excessive_buffer_overrun_errors` | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `local_link_integrity_errors`     | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `rnr_nak_retry_err`               | `hw_counters/` | Delta > 0         | **REPLACE_VM** | Yes          |

### Fatal Driver/Firmware Logs (IsFatal = true)

Certain kernel logs indicate a deterministic hardware/driver failure (see [Syslog Detection & Correlation](./syslog-detection-correlation.md)):

| Pattern                | Recommended Action               | Rationale                                         |
|------------------------|----------------------------------|---------------------------------------------------|
| **cmd_exec timeout**   | **RecommendedAction_REPLACE_VM** | Control plane broken, driver cannot manage device |
| **health poll failed** | **RecommendedAction_REPLACE_VM** | Firmware heartbeat lost, device non-functional    |
| **unrecoverable**      | **RecommendedAction_REPLACE_VM** | Hardware admission of failure                     |

### Non-Fatal Counters (Default: IsFatal = false)

| Counter                 | Path           | Default Threshold | Default Action | Configurable |
|-------------------------|----------------|-------------------|----------------|--------------|
| `symbol_error`          | `counters/`    | > 10/sec          | Monitor        | Yes          |
| `link_error_recovery`   | `counters/`    | > 5/min           | Monitor        | Yes          |
| `port_rcv_errors`       | `counters/`    | > 10/sec          | Monitor        | Yes          |
| `port_xmit_discards`    | `counters/`    | > 100/sec         | Monitor        | Yes          |
| `roce_slow_restart`     | `hw_counters/` | > 10/sec          | Monitor        | Yes          |
| `local_ack_timeout_err` | `hw_counters/` | > 1/sec           | Monitor        | Yes          |
| `carrier_changes`       | interface      | > 2/interval      | Monitor        | Yes          |

> **Note**: `rnr_nak_retry_err` is **FATAL** by default (see Fatal Counters table above). All counters can have their severity and threshold overridden via configuration.

### Design Principle

| Source                                        | IsFatal | Recommended Action | Purpose                            |
|-----------------------------------------------|---------|--------------------|------------------------------------|
| **Deterministic Logs**                        | `true`  | `REPLACE_VM`       | Fatal driver/firmware condition    |
| **Port State Changes** (link-state-detection) | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Fatal Counters** (link-counter-detection)   | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Diagnostic Logs**                           | `false` | `NONE`             | Evidence/context for investigation |

> **Key Insight**: Deterministically fatal events in logs (cmd_exec timeout, etc.) are **Fatal (IsFatal=true)** with `RecommendedAction_REPLACE_VM`. Diagnostic logs (insufficient power, High Temperature, module absent) are **Warning (IsFatal=false)**. State and counter conditions are also **Fatal (IsFatal=true)** with `RecommendedAction_REPLACE_VM`.

---

## References

### PHY & Signal Integrity
1. [PAM4 Error Correction Challenges in 400GbE (EDN)](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/)
2. [Determine Which Links Are Experiencing Significant Errors - Sun/Oracle (citing IBTA BER Threshold)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)

### Linux Kernel & Driver
3. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)

### Fabric Diagnostics
4. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)
5. [Black Hole Detection (sFlow)](https://blog.sflow.com/2016/05/black-hole-detection.html)
6. [InfiniBand™ Architecture Specification (IBTA)](https://www.infinibandta.org/ibta-specification/)

### Vendor Monitoring Guides
7. [InfiniBand Errors Dashboard - HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US)
8. [HPC Clusters Using InfiniBand on IBM Power Systems - IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)
9. [Health Checks for HPC Workloads on Microsoft Azure](https://techcommunity.microsoft.com/blog/azurehighperformancecomputingblog/health-checks-for-hpc-workloads-on-microsoft-azure/837843)
10. [NVIDIA DOCA Telemetry Service Guide](https://docs.nvidia.com/doca/archive/2-9-3/doca+telemetry+service+guide/index.html)
11. [Datadog InfiniBand Integration](https://docs.datadoghq.com/integrations/infiniband/)

---
