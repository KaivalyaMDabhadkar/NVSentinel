# MongoDB Migration: Bitnami to Percona Operator

This runbook describes how to move an existing NVSentinel installation from the Bitnami MongoDB backend (the current default) to the Percona Operator backend.

Common reasons to migrate:

- Your cluster has ARM64 nodes. The Bitnami MongoDB images are only published for amd64, so the default backend cannot run there. The Percona images are multi-arch. See [MongoDB Store Configuration](../configuration/mongodb-store.md) for details.
- You want the operator features described in [ADR-013](../designs/013-mongodb-bitnami-migration.md), such as automated lifecycle management and integrated backups.

The commands below assume the release name `nvsentinel` in the namespace `nvsentinel`. Adjust both if your installation differs.

## What to expect

- **Health event data is not preserved**, unless you use the optional restore path described in [Preserving fault state](#preserving-fault-state-optional-dump-and-restore). The default procedure is a clean reinstall of the datastore, not a data migration. NVSentinel stores operational telemetry (health events, resume tokens, maintenance events). Monitors repopulate the new database as they detect issues, but all history is lost.
- **Do not switch backends with a plain `helm upgrade` on an existing release.** Changing `useBitnami`/`usePerconaOperator` on a live release fails with an immutable field error on the `create-mongodb-database` Job, and by that point the upgrade has already deployed parts of the second backend. You end up with two MongoDB clusters running side by side and a service configuration that points at the broken one. The uninstall and reinstall flow below is the supported path. If you already hit this, see [Troubleshooting](#troubleshooting).
- Plan a maintenance window. Between the uninstall and the completed reinstall, NVSentinel is not monitoring the cluster.

## Before you begin

You need `helm` and `kubectl` access to the cluster, and cert-manager must be installed (both backends use it for TLS certificates).

On a production cluster, check for in-flight fault handling before you start. Health events are referenced by their database IDs from node annotations and remediation resources. After the database is wiped, those references point at nothing. In particular, node-drainer looks up the event behind the `quarantineHealthEvent` node annotation and retries every minute, forever, when the event is missing. Nodes in that state never drain and never recover on their own.

Step 3 below removes this state. Be aware of the operational consequence: clearing quarantine state returns the affected nodes to service, and not every fault will be detected a second time. Faults that are still observable, such as a failing DCGM health check or a NIC that is still down, are re-detected on the next monitoring cycle. One-time events, such as GPU XID errors that were already read from the logs, will not be raised again. Record the list of quarantined nodes before you start (step 3 shows how) and review each one manually after the migration: remediate it, or keep it cordoned until you are confident it is healthy.

## Helper scripts

The repository ships scripts that implement the mechanical parts of this runbook under `scripts/mongodb-migration/`:

| Script | Step | What it does |
| ------ | ---- | ------------ |
| `preflight.sh` | before you begin | Read-only readiness check: current backend, cert-manager, storage class minimums vs the requested volume size, quarantined nodes, in-flight remediation objects. Prints a verdict table; exits 2 when blocked. |
| `cleanup.sh` | steps 2 and 3 | Deletes everything `helm uninstall` leaves behind, then verifies nothing remains. Refuses to run while the release is still installed and asks for confirmation. `--dry-run` prints the plan; `--clear-fault-state` also clears node annotations and remediation objects. |
| `migrate-data.sh` | optional | `dump` streams a mongodump archive out of the old backend (always excluding `ResumeTokens`); `restore` streams it into the new one. Document IDs are preserved, so node annotations and remediation resource names stay valid. |
| `verify.sh` | step 6 | Waits on the five post-install gates and prints a verdict table. |

All four respect `NVSENTINEL_NAMESPACE` and `NVSENTINEL_RELEASE` (default `nvsentinel` for both). The scripts automate the steps; the decisions (acknowledging data loss, reviewing quarantined nodes) stay with you.

For AI-agent-assisted runs of this procedure, `skills/mongodb-migration/bitnami-to-percona/` contains agent skills (readiness check, migration, verification) that sequence these scripts with the required confirmation gates. They work with any agent that reads the SKILL.md format.

## GitOps-managed installations (ArgoCD, Flux)

If NVSentinel is deployed by a GitOps controller, stop reconciliation before you start. Otherwise the controller fights the migration: it re-syncs the resources you just removed and reverts the backend change. These are general guidelines; adapt them to how your applications are structured.

Two workable patterns:

**Pause and operate.** Suspend reconciliation for the NVSentinel application (ArgoCD: disable automated sync, or use a sync window that denies syncs for the duration; Flux: `flux suspend helmrelease <name>`). Perform the migration manually following this runbook. Update the desired state in git (the two backend flags, the volume size if your provider needs it, scheduling values for the Percona components), then resume reconciliation and confirm the first sync reports no drift.

**Delete and recreate.** Delete the NVSentinel application from the controller so its resources are removed (this replaces step 1 of this runbook), run the cleanup and state-handling steps manually, update the desired state in git, then recreate the application and let it deploy the Percona-backed installation. Run the verification of step 6 against the result.

Notes that apply to both patterns:

- Update git before reconciliation resumes. If the old values come back first, the sync flips the backend in place, which is the unsupported switch this runbook exists to avoid.
- A GitOps-rendered installation is not necessarily a Helm release (ArgoCD renders Helm charts without creating release records), so `helm uninstall` may have nothing to act on; removing the application is the equivalent step. The cleanup script works the same either way.
- If the application manages the namespace with pruning enabled, double check that the objects this runbook creates or preserves (the pull secrets, a dump archive stored in-cluster, and on the restore path the node annotations and remediation resources) are not pruned as unmanaged resources when reconciliation resumes.

## Preserving fault state (optional dump and restore)

By default this migration drops all health event data. If the cluster has active quarantines or in-flight remediations, consider the restore path instead: it carries the health events into the new backend with their document IDs intact, so the node annotations and remediation resources that reference those IDs stay valid, and fault handling picks up where it left off. In testing, a node quarantined before the migration stayed quarantined through it, node-drainer resolved the event behind the annotation without errors, and fault-remediation recognized its existing maintenance resource on cold start instead of creating a duplicate.

The restore path is the same procedure with three changes:

1. **Dump before uninstalling** (while the old backend is still up):

   ```bash
   scripts/mongodb-migration/migrate-data.sh dump /path/to/pre-migration.archive
   ```

   The script detects the current backend and always excludes the
   `ResumeTokens` collection. Resume tokens are only valid on the cluster
   that created them and must never travel; consumers write fresh ones on
   their next start. Confirm the reported archive size is non-zero before
   moving on.

2. **Keep the fault-handling state.** Skip step 3 entirely: do not clear the
   node annotations, do not delete the remediation resources or the
   log-collector jobs. Restored documents keep their IDs, so all of those
   references become valid again after the restore. (Steps 1 and 2, the
   uninstall and the datastore cleanup, still apply unchanged.)

3. **Restore after verification, then restart the consumers.** Once step 6
   passes:

   ```bash
   scripts/mongodb-migration/migrate-data.sh restore /path/to/pre-migration.archive
   ```

   ```bash
   kubectl rollout restart deploy/health-events-analyzer deploy/fault-quarantine deploy/node-drainer deploy/fault-remediation -n nvsentinel
   ```

   Restart only the deployments that exist in your installation. The
   restart matters: the consumers read their connection configuration at
   startup, and their cold-start logic is what processes the restored
   events.

The dump deliberately includes resolved and already remediated events, and
that is safe. Fault-remediation's cold start only enqueues events whose
remediation is incomplete, so finished events are never re-processed.
Remediation resource names are derived from the event ID, so even a
re-processed event finds its existing resource instead of creating a
duplicate. And the health events collection carries a TTL index on the
creation timestamp, so restored history ages out on its original schedule.
Keeping the history also preserves the analyzer's context for rules that
consider past events.

Behaviors to expect with the restore path:

- **The event exporter re-exports everything**, including resolved events.
  With no resume token in the new datastore, its backfill treats every
  restored event as new. In testing this was an exact one-to-one re-export
  of the whole collection. Warn the owners of the downstream sink to
  expect duplicates.
- **Restored quarantine state is latent.** Fault handling is event driven,
  so a restored quarantine is re-evaluated when the next event or a
  component cold start touches that node, not spontaneously. This is
  normal; the state is correct and consistent, it just does not generate
  activity on its own.
- **The CSP maintenance replay does not happen on this path.** The restored
  maintenance events carry the CSP health monitor's progress watermark, so
  it resumes from where it left off instead of re-ingesting the provider's
  feed the way the plain path does.

## Step 1: Uninstall the release

```bash
helm uninstall nvsentinel -n nvsentinel
```

## Step 2: Delete the datastore leftovers

`helm uninstall` intentionally leaves several objects behind:

| Leftover | Why it survives |
| -------- | --------------- |
| `datadir-mongodb-*` PVCs | StatefulSet volume claims are never deleted by Helm |
| `mongodb` secret | Carries a `helm.sh/resource-policy: keep` annotation |
| `mongo-root-ca-secret`, `mongo-app-client-cert-secret`, `mongo-server-cert-*` secrets | Created by cert-manager, which does not remove secrets when Certificates are deleted |
| `mongo-ca-secret` secret | Created by an init job outside of Helm ownership |
| `resume-control` ConfigMap | Created at runtime by health-events-analyzer and other datastore consumers |
| `circuit-breaker` ConfigMap | Created at runtime by fault-quarantine |

Delete all of them:

```bash
kubectl delete pvc -l app.kubernetes.io/name=mongodb -n nvsentinel
```

```bash
kubectl delete secret mongodb mongo-ca-secret mongo-root-ca-secret mongo-app-client-cert-secret mongo-server-cert-0 mongo-server-cert-1 mongo-server-cert-2 -n nvsentinel --ignore-not-found=true
```

There is one `mongo-server-cert-<n>` secret per MongoDB replica. The default replica count is 3. If you changed `mongodb-store.mongodb.replicaCount`, adjust the list.

```bash
kubectl delete configmap resume-control circuit-breaker -n nvsentinel --ignore-not-found=true
```

Do not skip the secret cleanup. The old TLS secrets belong to the old certificate authority. If they survive, the new installation reuses them, clients present certificates the new database does not trust, and every datastore consumer crash-loops with connection errors that do not mention certificates at all.

Do not delete your image pull secrets or any other secrets you created yourself.

## Step 3: Clear fault-handling state (production clusters)

Skip this step entirely if you are following the [restore path](#preserving-fault-state-optional-dump-and-restore); the restored data makes this state valid again. Otherwise, skip it only if you are certain there were no active quarantines or remediations.

First, record which nodes are currently quarantined so you can review them after the migration. This matters because one-time events (GPU XIDs, for example) will not be detected again once the event history is gone:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,QUARANTINE:.metadata.annotations.quarantineHealthEvent --no-headers | grep -v "<none>"
```

Then remove the quarantine and remediation annotations from all nodes:

```bash
kubectl annotate nodes --all quarantineHealthEvent- quarantineHealthEventAppliedTaints- quarantineHealthEventAppliedLabels- quarantineHealthEventIsCordoned- quarantineHealthEventCordonPreExisting- latestFaultRemediationState-
```

If NVSentinel cordoned or tainted nodes as part of a quarantine, uncordon them and remove the taints as well, or leave them cordoned if you prefer to review each node manually first.

Delete remediation resources whose names and specs reference old event IDs:

```bash
kubectl delete rebootnodes,terminatenodes,gpuresets --all -n nvsentinel --ignore-not-found=true
```

Note that these remediation resources are cluster scoped; kubectl ignores the namespace flag for them (with a warning) and `--all` deletes every instance on the cluster. For NVSentinel they all belong to the one installation, so that is the intended behavior.

```bash
kubectl delete externalremediationrequests --all -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete jobs -l dgxc.nvidia.com/event-id -n nvsentinel
```

If you use a custom drain plugin, also delete its in-flight drain resources (their names start with `drain-`).

Leave the health monitor state files on the nodes alone (for example the syslog monitor state under `/var/run/syslog_monitor/`). They only contain local journal cursors and boot IDs, not database references. Deleting them causes old log entries to be detected again as new faults.

## Step 4: Prepare the Percona values

Add the backend flags to your values. Both flags must be set:

```yaml
mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

Three more settings commonly need attention:

**Volume size.** The Percona defaults request 8Gi volumes. Some cloud providers have a larger minimum block volume size. On OCI, for example, block volumes are at least 50Gi, the CSI driver rounds the volume up, and the operator then refuses to reconcile because the volume is larger than requested. The replica set never initializes. Set the size explicitly to at least your provider's minimum:

```yaml
mongodb-store:
  psmdb-db:
    replsets:
      rs0:
        volumeSpec:
          pvc:
            resources:
              requests:
                storage: "50Gi"
```

**Pod placement.** The Percona components have their own scheduling keys. Values under `mongodb-store.mongodb.*` do not apply to them:

```yaml
mongodb-store:
  job:
    nodeSelector: {}
    tolerations: []
  psmdb-operator:
    nodeSelector: {}
    tolerations: []
  psmdb-db:
    replsets:
      rs0:
        nodeSelector: {}
        tolerations: []
```

**Connection endpoint.** The two backends expose different services: Bitnami uses `mongodb-headless`, Percona uses `mongodb-rs0`. If you rely on the chart-generated `MONGODB_URI` (the default), nothing to do, it switches automatically with the backend flags. If you set `global.datastore.connection.host` yourself, update it to `mongodb-rs0.<namespace>.svc.cluster.local`.

## Step 5: Install with Percona enabled

```bash
helm upgrade --install nvsentinel <chart> -n nvsentinel -f <your-values.yaml> --timeout 20m --wait --wait-for-jobs
```

The `--wait-for-jobs` flag makes Helm wait for the `create-mongodb-database` job as well; `--wait` alone only waits for the workloads.

The install takes longer than the Bitnami one because the operator starts first, then builds the replica set, and only then can the database initialization job finish. As a reference point from validation runs: the operator was up within a minute, the replica set reached `ready` about 3 minutes in, the initialization job completed about 2 minutes after that, and the NVSentinel services settled shortly after. The whole install typically finishes well within 10 minutes; the 20-minute timeout leaves room for slow image pulls.

## Step 6: Verify

Check that the operator has reconciled the replica set:

```bash
kubectl get perconaservermongodb -n nvsentinel
```

The status must be `ready`. Then check the initialization job and the pods:

```bash
kubectl get jobs,pods -n nvsentinel
```

Expected state: the `create-mongodb-database` job shows `Complete`, the `mongodb-rs0-0/1/2` pods are `Running`, and the NVSentinel services are `Running`. It is normal for the services to restart a few times while the replica set comes up; they crash until the database answers and then stay up. Confirm connectivity from the logs:

```bash
kubectl logs -n nvsentinel deploy/health-events-analyzer | grep "Successfully pinged"
```

## After the migration

- **CSP maintenance events replay (plain path only).** The CSP health monitor tracks its progress through the provider's maintenance feed using the database itself. With the database wiped, it re-ingests every maintenance event still visible in the provider API, which can re-quarantine nodes for maintenance you already handled. Expect this for one polling cycle after the migration. On the restore path this does not happen, because the restored maintenance events preserve the watermark.
- **Event exporter backfill.** With no resume token in the new database, the event exporter starts from scratch. Against an empty database this is a no-op. If you restore or seed any data, it exports all of it again, so warn the owners of the downstream sink about duplicates.
- **Quarantine history is gone.** Nodes you cleared in step 3 are back in service. Faults that are still observable (persistent hardware conditions, failing health checks) are re-detected and re-quarantined on the next monitoring cycle. One-time events such as GPU XID errors are not raised again, so work through the list of quarantined nodes you recorded in step 3 and handle those nodes manually.

## Troubleshooting

| Symptom | Cause | Fix |
| ------- | ----- | --- |
| `mongodb-0` stuck in `Init:ImagePullBackOff`, events show `no match for platform in manifest` | Bitnami MongoDB images have no ARM64 build | Use the Percona backend on ARM64 nodes |
| Operator logs show `requested storage (8Gi) is less than actual storage (...)`, the `perconaservermongodb` resource stays in `error`, services report `ReplicaSetNoPrimary` | Cloud provider minimum volume size is larger than the requested size, so reconciliation stops before the replica set is initialized | Set `volumeSpec` to at least the provider minimum (step 4), delete the `mongod-data-*` PVCs, reinstall |
| All datastore consumers crash-loop with connection errors right after the reinstall, TLS handshakes fail or connections close immediately | Leftover TLS secrets from the previous backend were not deleted, so client certificates belong to the old certificate authority | Redo step 2 completely, then reinstall |
| `helm upgrade` fails with `cannot patch "create-mongodb-database" ... field is immutable` | Backend flags were changed on an existing release instead of following this runbook | Run `helm rollback <release> <last-good-revision>` (retry once if it errors), then manually delete everything the failed upgrade created: the `perconaservermongodb` resource, the `nvsentinel-psmdb-operator` deployment and its ServiceAccount, Role and RoleBinding, the `mongod-data-*` PVCs, and the `internal-mongodb-users`, `percona-server-mongodb-users` and `mongodb-encryption-key` secrets. Helm no longer tracks these objects after the rollback, so `helm uninstall` will not remove them either. Then follow this runbook from step 1 |
| node-drainer logs repeat `unexpected number of events for node ... and event ID ...` every minute | A node still carries a `quarantineHealthEvent` annotation that references an event from the old database | Run the annotation cleanup from step 3 |

## Rolling back

Going back to Bitnami is the same procedure in the other direction: uninstall, delete the Percona leftovers, and reinstall with `useBitnami: true` and `usePerconaOperator: false`. Data is not preserved in this direction either.

The Percona leftovers to delete after the uninstall:

```bash
kubectl delete pvc mongod-data-mongodb-rs0-0 mongod-data-mongodb-rs0-1 mongod-data-mongodb-rs0-2 -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete secret internal-mongodb-users percona-server-mongodb-users mongodb-keyfile mongodb-encryption-key mongodb-ssl mongodb-ssl-internal mongodb-ca-cert mongo-app-client-cert-secret -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete configmap resume-control circuit-breaker -n nvsentinel --ignore-not-found=true
```

The Percona CRDs (`perconaservermongodbs.psmdb.percona.com` and related) are cluster-scoped and survive the uninstall. That is harmless: they are reused if you install Percona again, and they can stay in place while you run Bitnami. Delete them only if you want a complete teardown and nothing else on the cluster uses them.

Note that a rollback is not possible on ARM64-only clusters, because the Bitnami images do not run there.
