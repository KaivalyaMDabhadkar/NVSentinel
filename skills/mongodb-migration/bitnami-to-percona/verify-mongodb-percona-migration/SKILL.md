---
name: verify-mongodb-percona-migration
description: >-
  Post-migration verification for the NVSentinel Percona MongoDB backend:
  waits on the five install gates, performs the optional data restore and
  consumer restarts, and walks the operator through the aftermath
  expectations. Use after migrate-mongodb-to-percona completes, or on its
  own to health-check an existing Percona-backed installation.
maturity: experimental
lifecycle: evergreen
api-version: nvsentinel.skills/v1
allowed-tools: Bash(kubectl *), Bash(helm *), Bash(scripts/mongodb-migration/*), Read, Grep
---

# Verify MongoDB Percona Migration

## When to use

Use this skill **after** `migrate-mongodb-to-percona`, or standalone to
health-check a Percona-backed installation.

## Inputs

- `NVSENTINEL_NAMESPACE` (default `nvsentinel`)
- `DATA_PATH` (`restore` is the default, `clean` is the opt-out) and
  `ARCHIVE` (restore path only), carried over from the migration skill

## Setup

```bash
export NVSENTINEL_NAMESPACE="nvsentinel"
```

## Steps

1. **Run the gates:**

   ```bash
   scripts/mongodb-migration/verify.sh
   ```

   Five gates: psmdb resource `ready`, initialization job `Complete`, mongod
   pods ready, `MONGODB_URI` pointing at `mongodb-rs0`, and a consumer
   logging `Successfully pinged`. All five must pass. Gate five requires at
   least one of health-events-analyzer or fault-quarantine to be deployed;
   the script fails the gate when neither exists, because connectivity
   cannot be confirmed. Timeouts are tunable
   via `VERIFY_CR_TIMEOUT`, `VERIFY_JOB_TIMEOUT`, `VERIFY_POD_TIMEOUT`,
   `VERIFY_PING_RETRIES`. A consumer stuck in `CrashLoopBackOff` with old
   restart counts may just be in backoff from the bring-up window; delete
   the pod to skip the backoff before diagnosing.

2. **Restore (default preserve path; skipped only on the clean path):**

   ```bash
   scripts/mongodb-migration/migrate-data.sh restore <ARCHIVE>
   ```

   Then restart the datastore consumers so their cold-start logic processes
   the restored events:

   ```bash
   for D in health-events-analyzer fault-quarantine node-drainer fault-remediation; do
     kubectl get deploy "$D" -n "$NVSENTINEL_NAMESPACE" >/dev/null 2>&1 && \
       kubectl rollout restart deploy "$D" -n "$NVSENTINEL_NAMESPACE"
   done
   ```

   The loop restarts only the deployments that exist in this installation.
   Rollout restarts are not spec changes, so they create no GitOps drift.

3. **Restore continuity checks (restore path only):**
   - Quarantined nodes are still cordoned and their `quarantineHealthEvent`
     annotations reference documents that exist in the new datastore.
   - node-drainer logs show NO repeating
     `unexpected number of events for node ...` lines (that loop means an
     annotation references a missing document).
   - `ResumeTokens` contains only freshly written tokens (the restore never
     carries the old ones).

4. **Aftermath.** Walk the operator through what to expect either way:
   - Restore path (default): the event exporter has no resume token, so it
     re-exports every restored event to its sink; warn the downstream
     owners about duplicates. Restored quarantine state is latent until
     the next event or cold start touches the node. There is NO CSP
     maintenance replay, because the restored maintenance events preserve
     the watermark.
   - Clean path: the CSP health monitor re-ingests provider maintenance
     events still visible in the provider API (its watermark lived in the
     wiped database); expect one polling cycle of replay. One-time faults
     (GPU XIDs) are not re-detected; the operator must work through the
     quarantined-node list recorded during readiness. Persistent faults
     are re-detected on the next monitoring cycle.

## Output

Report to the operator:

- the verify.sh verdict table
- restore performed or skipped, and the continuity check results
- the aftermath items that apply to this installation

## Next skill to run

None; the migration is complete when all gates pass and, on the restore
path, the continuity checks hold. If a gate fails, the failure-branch table
in `migrate-mongodb-to-percona` and the runbook troubleshooting table are
the recovery references.

## References

| Topic | Reference |
|-------|-----------|
| Runbook (source of truth) | `docs/runbooks/mongodb-bitnami-to-percona-migration.md` |
| Scripts | `scripts/mongodb-migration/` |
| Migration skill | [migrate-mongodb-to-percona](../migrate-mongodb-to-percona/SKILL.md) |
