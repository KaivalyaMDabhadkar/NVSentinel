---
name: check-mongodb-migration-readiness
description: >-
  Readiness check for migrating an NVSentinel installation from the Bitnami
  MongoDB backend to the Percona Operator backend. Runs the preflight script,
  interprets the verdict table, and captures the operator decisions (data
  handling, quarantined nodes, GitOps) that the migration skill requires.
  Use this first, before any migration step.
maturity: experimental
lifecycle: evergreen
api-version: nke.skills/v1
allowed-tools: Bash(kubectl *), Bash(helm *), Bash(scripts/mongodb-migration/preflight.sh*), Read, Grep
---

# Check MongoDB Migration Readiness

## When to use

Use this skill **before** `migrate-mongodb-to-percona`. Nothing in this skill
mutates the cluster.

The procedure it gates is documented in
`docs/runbooks/mongodb-bitnami-to-percona-migration.md` (the source of truth).

## Inputs

- `NVSENTINEL_NAMESPACE` (default `nvsentinel`)
- `NVSENTINEL_RELEASE` (default `nvsentinel`)
- `MIGRATION_PVC_SIZE_GI` (default `8`): the volume size the Percona install
  will request. Set it to the value from the operator's planned values file
  so the storage check validates the real request.

## Setup

```bash
export NVSENTINEL_NAMESPACE="nvsentinel"
export NVSENTINEL_RELEASE="nvsentinel"
export MIGRATION_PVC_SIZE_GI="8"
```

## Steps

1. Run the preflight script and show the operator the full table:

   ```bash
   scripts/mongodb-migration/preflight.sh
   ```

2. Interpret the verdict:
   - Exit `2` (`BLOCKED`): stop. Help the operator resolve every FAIL row,
     then re-run. Typical blockers: a failed/pending Helm release, both
     backends present (mixed state; see the runbook troubleshooting table),
     cert-manager missing, or the default StorageClass minimum above the
     requested volume size (OCI block volumes have a 50Gi minimum; the
     Percona operator wedges before replica set init when the provisioned
     volume is larger than requested).
   - Exit `0` with REVIEW rows: each REVIEW row is an operator decision,
     not yours. Do not proceed until each is acknowledged in conversation.

3. Capture the decisions the migration skill needs:
   - **Data handling:** plain path (all health event data lost) or
     dump/restore path (document IDs preserved; node annotations and
     remediation resources stay valid; in-flight fault handling resumes
     after restore). On clusters with active quarantines, recommend
     dump/restore: with the plain path, one-time faults such as GPU XIDs
     are never re-detected. The restore path additionally requires scaling
     fault-quarantine, node-drainer, and fault-remediation to zero before
     the dump, so no references get created for events the archive will
     not contain; capture that as part of the plan.
   - **Quarantined nodes:** record the list from the REVIEW row. With the
     plain path the operator must decide per node: remediate first, keep
     cordoned for manual review, or return to service.
   - **GitOps:** ask whether ArgoCD/Flux manages the installation. If yes,
     reconciliation must be suspended before the migration and the desired
     state in git updated before resuming (runbook, GitOps section).

## Output

Report to the operator:

- verdict (`READY` / `READY with review items` / `BLOCKED`)
- the chosen data-handling path
- the recorded quarantined-node list (or "none")
- whether GitOps suspension is required
- the values changes the install will need (backend flags, volume size,
  scheduling keys)

## Next skill to run

- `migrate-mongodb-to-percona` (only on `READY`, with the decisions above
  captured)

## References

| Topic | Reference |
|-------|-----------|
| Runbook (source of truth) | `docs/runbooks/mongodb-bitnami-to-percona-migration.md` |
| Backend configuration | `docs/configuration/mongodb-store.md` |
| Scripts | `scripts/mongodb-migration/` |
