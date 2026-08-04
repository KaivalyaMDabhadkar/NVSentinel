---
name: migrate-mongodb-to-percona
description: >-
  Execute the NVSentinel MongoDB backend migration from Bitnami to the
  Percona Operator after readiness is confirmed: optional data dump,
  uninstall, cleanup of surviving objects, values preparation, and the
  Percona install. Destructive: wipes health event data unless the dump
  path was chosen. Use only after check-mongodb-migration-readiness
  reports READY and the operator confirmed the decisions.
maturity: experimental
lifecycle: evergreen
api-version: nke.skills/v1
allowed-tools: Bash(kubectl *), Bash(helm *), Bash(scripts/mongodb-migration/*), Read, Grep
---

# Migrate MongoDB to Percona

## When to use

Use this skill **after** `check-mongodb-migration-readiness` reports READY.

Hard gate: do NOT run any step below until the operator has confirmed, in
this conversation, (a) the data-handling decision (data loss accepted, or
dump path chosen) and (b) what happens to each quarantined node. If either
is missing, go back to the readiness skill.

## Inputs

- `NVSENTINEL_NAMESPACE`, `NVSENTINEL_RELEASE` (default `nvsentinel`)
- `DATA_PATH` (`plain` / `restore`): the confirmed data-handling decision
- `VALUES_FILES`: the operator's values file(s) for the install
- `ARCHIVE`: dump archive path (restore path only)
- GitOps status from readiness (if managed: reconciliation suspended before
  step 1, git updated before resuming; runbook GitOps section)

## Setup

```bash
export NVSENTINEL_NAMESPACE="nvsentinel"
export NVSENTINEL_RELEASE="nvsentinel"
```

## Trigger order (required)

1. **Dump (restore path only):**

   ```bash
   scripts/mongodb-migration/migrate-data.sh dump <ARCHIVE>
   ```

   The script auto-detects the source backend and always excludes
   `ResumeTokens` (change-stream tokens are only valid on the cluster that
   created them). Gate: the script must report a non-empty archive.

2. **Uninstall:**

   ```bash
   helm uninstall "$NVSENTINEL_RELEASE" -n "$NVSENTINEL_NAMESPACE"
   ```

3. **Cleanup:**

   ```bash
   scripts/mongodb-migration/cleanup.sh --yes
   ```

   Add `--clear-fault-state` ONLY on the plain path. On the restore path,
   fault state (node annotations, remediation resources) must be kept:
   restored documents keep their IDs, so those references become valid
   again. Gate: exit 0. A non-zero exit means leftovers remain; leftover
   TLS secrets cause opaque connection-closed crash loops on the next
   install. Do not continue until cleanup verifies clean.

4. **Values.** Confirm the operator's values contain:
   - `mongodb-store.useBitnami: false` and
     `mongodb-store.usePerconaOperator: true` (both, always together)
   - volume size at or above the provider minimum (OCI block volumes: 50Gi):
     `mongodb-store.psmdb-db.replsets.rs0.volumeSpec.pvc.resources.requests.storage`
   - scheduling for `mongodb-store.job`, `mongodb-store.psmdb-operator`,
     and `mongodb-store.psmdb-db.replsets.rs0` where the cluster uses node
     selectors or taints
   - on single-node test clusters only:
     `mongodb-store.psmdb-db.replsets.rs0.affinity.antiAffinityTopologyKey: "none"`
     (the psmdb default is required anti-affinity across hostnames)

5. **Install:**

   ```bash
   helm upgrade --install "$NVSENTINEL_RELEASE" <chart> -n "$NVSENTINEL_NAMESPACE" \
     -f <VALUES_FILES...> --timeout 20m --wait --wait-for-jobs
   ```

   `--wait-for-jobs` matters: `--wait` alone does not wait for the database
   initialization job.

## Failure branches (validated)

| Symptom | Action |
| ------- | ------ |
| Operator log `requested storage (...) is less than actual storage (...)`, psmdb `error`, no primary | Volume below the provider minimum. Fix values, delete the `mongod-data-*` PVCs, re-run step 5. |
| `create-mongodb-database` job `Failed` with `DeadlineExceeded` | The job never retries on its own. `kubectl delete job create-mongodb-database -n "$NVSENTINEL_NAMESPACE"`, then re-run the same `helm upgrade`; Helm recreates it and it completes against the healthy replica set. |
| Consumers crash-loop with TLS/connection-closed errors right after install | Leftover TLS secrets from the old backend. Re-run step 3 fully, then step 5. |
| `helm upgrade` fails with `field is immutable` on the create-mongodb-database Job | An in-place backend switch was attempted on a live release. Follow the runbook troubleshooting row (rollback, manual deletion of everything the failed revision created), then restart from the readiness skill. |

## Next skill to run

- `verify-mongodb-percona-migration` (always; it also performs the restore
  on the restore path)

## References

| Topic | Reference |
|-------|-----------|
| Runbook (source of truth) | `docs/runbooks/mongodb-bitnami-to-percona-migration.md` |
| Scripts | `scripts/mongodb-migration/` |
| Readiness | [check-mongodb-migration-readiness](../check-mongodb-migration-readiness/SKILL.md) |
