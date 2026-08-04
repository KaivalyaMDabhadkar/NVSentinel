#!/usr/bin/env bash
#
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# preflight.sh - readiness check for the NVSentinel MongoDB Bitnami -> Percona migration.
# Read-only: performs no mutations. Prints a verdict table and exits:
#   0 = READY (review items may still need human attention)
#   2 = BLOCKED (at least one FAIL row)
#   1 = script/environment error
set -uo pipefail

NS="${NVSENTINEL_NAMESPACE:-nvsentinel}"
RELEASE="${NVSENTINEL_RELEASE:-nvsentinel}"
# Volume size the Percona install will request (chart default 8Gi unless overridden).
TARGET_PVC_SIZE_GI="${MIGRATION_PVC_SIZE_GI:-8}"

FAIL=0
REVIEW=0
ROWS=()

row() { # status | check | detail
  ROWS+=("$1|$2|$3")
  case "$1" in
    FAIL) FAIL=$((FAIL + 1)) ;;
    REVIEW) REVIEW=$((REVIEW + 1)) ;;
  esac
}

# --- 1. cluster reachable -----------------------------------------------------
if ! kubectl get ns "$NS" >/dev/null 2>&1; then
  echo "ERROR: cannot reach the cluster or namespace '$NS' does not exist." >&2
  exit 1
fi
row PASS "Cluster access" "namespace $NS reachable"

# --- 2. helm release ----------------------------------------------------------
REL_STATUS="$(helm status "$RELEASE" -n "$NS" -o json 2>/dev/null | grep -o '"status":"[a-z-]*"' | head -1 | cut -d'"' -f4)"
if [ -z "$REL_STATUS" ]; then
  row REVIEW "Helm release" "release '$RELEASE' not found in $NS (already uninstalled? cleanup can still run)"
elif [ "$REL_STATUS" = "deployed" ]; then
  row PASS "Helm release" "release '$RELEASE' status=deployed"
else
  row FAIL "Helm release" "release '$RELEASE' status=$REL_STATUS (resolve failed/pending release before migrating)"
fi

# --- 3. current backend -------------------------------------------------------
HAS_BITNAMI="no"; HAS_PERCONA="no"
kubectl get statefulset mongodb -n "$NS" >/dev/null 2>&1 && HAS_BITNAMI="yes"
kubectl get psmdb mongodb -n "$NS" >/dev/null 2>&1 && HAS_PERCONA="yes"
if [ "$HAS_BITNAMI" = "yes" ] && [ "$HAS_PERCONA" = "yes" ]; then
  row FAIL "Current backend" "BOTH backends present (mixed state, likely a failed in-place switch; see runbook troubleshooting)"
elif [ "$HAS_BITNAMI" = "yes" ]; then
  row PASS "Current backend" "Bitnami (statefulset/mongodb found)"
elif [ "$HAS_PERCONA" = "yes" ]; then
  row FAIL "Current backend" "Percona already active; nothing to migrate"
else
  row REVIEW "Current backend" "no MongoDB backend found in $NS (fresh install rather than migration?)"
fi

# --- 4. cert-manager ----------------------------------------------------------
CM_READY="$(kubectl get deploy -A -l app.kubernetes.io/name=cert-manager --no-headers 2>/dev/null | awk '{print $3}' | head -1)"
if [ -n "$CM_READY" ] && [ "${CM_READY%%/*}" != "0" ]; then
  row PASS "cert-manager" "deployment ready ($CM_READY)"
else
  row FAIL "cert-manager" "not found or not ready (both backends need it for TLS)"
fi

# --- 5. storage class vs requested volume size ---------------------------------
# NOTE: kubectl renders the default marker inside the NAME column ("standard (default)"),
# so on the default class the provisioner is field 3, not field 2.
DEFAULT_SC_LINE="$(kubectl get sc --no-headers 2>/dev/null | grep '(default)' | head -1)"
DEFAULT_SC="$(echo "$DEFAULT_SC_LINE" | awk '{print $1}')"
PROVISIONER="$(echo "$DEFAULT_SC_LINE" | awk '{print $3}')"
if [ -z "$DEFAULT_SC" ]; then
  row FAIL "StorageClass" "no default StorageClass (Percona PVCs will stay Pending)"
else
  MIN_GI=0
  case "$PROVISIONER" in
    *oraclecloud*|*oci*) MIN_GI=50 ;;
  esac
  if [ "$MIN_GI" -gt 0 ] && [ "$TARGET_PVC_SIZE_GI" -lt "$MIN_GI" ]; then
    row FAIL "StorageClass" "default '$DEFAULT_SC' ($PROVISIONER) has a ${MIN_GI}Gi minimum but the migration will request ${TARGET_PVC_SIZE_GI}Gi; set psmdb-db.replsets.rs0.volumeSpec.pvc.resources.requests.storage to at least ${MIN_GI}Gi (the operator otherwise wedges before replica set init)"
  else
    row PASS "StorageClass" "default '$DEFAULT_SC' ($PROVISIONER), requesting ${TARGET_PVC_SIZE_GI}Gi"
  fi
fi

# --- 6. quarantined nodes -----------------------------------------------------
QUARANTINED="$(kubectl get nodes -o custom-columns=NAME:.metadata.name,Q:.metadata.annotations.quarantineHealthEvent --no-headers 2>/dev/null | awk '$2 != "<none>" {print $1}')"
if [ -n "$QUARANTINED" ]; then
  row REVIEW "Quarantined nodes" "$(echo "$QUARANTINED" | tr '\n' ' ')(record this list; one-time events like XIDs will NOT be re-detected after the wipe)"
else
  row PASS "Quarantined nodes" "none"
fi

# --- 7. in-flight remediation objects ------------------------------------------
CR_COUNT=0
for KIND in rebootnodes terminatenodes gpuresets externalremediationrequests; do
  N="$(kubectl get "$KIND" -n "$NS" --no-headers 2>/dev/null | wc -l)"
  CR_COUNT=$((CR_COUNT + N))
done
JOB_COUNT="$(kubectl get jobs -n "$NS" -l dgxc.nvidia.com/event-id --no-headers 2>/dev/null | wc -l)"
if [ "$CR_COUNT" -gt 0 ] || [ "$JOB_COUNT" -gt 0 ]; then
  row REVIEW "In-flight remediation" "$CR_COUNT remediation CR(s), $JOB_COUNT log-collector job(s) reference old event IDs; cleanup.sh --clear-fault-state removes them"
else
  row PASS "In-flight remediation" "none"
fi

# --- 8. leftovers from a previous migration attempt ----------------------------
STALE=""
for S in internal-mongodb-users percona-server-mongodb-users mongodb-encryption-key mongodb-keyfile; do
  kubectl get secret "$S" -n "$NS" >/dev/null 2>&1 && STALE="$STALE $S"
done
if [ -n "$STALE" ]; then
  row REVIEW "Stale Percona objects" "found:$STALE (from an earlier attempt; delete before installing Percona)"
else
  row PASS "Stale Percona objects" "none"
fi

# --- 9. runtime state configmaps (informational) --------------------------------
STATE_CMS=""
for C in resume-control circuit-breaker; do
  kubectl get configmap "$C" -n "$NS" >/dev/null 2>&1 && STATE_CMS="$STATE_CMS $C"
done
if [ -n "$STATE_CMS" ]; then
  row PASS "Runtime state" "present:$STATE_CMS (cleanup.sh deletes these)"
else
  row PASS "Runtime state" "none present"
fi

# --- report --------------------------------------------------------------------
echo
echo "NVSentinel MongoDB migration preflight (namespace=$NS release=$RELEASE)"
echo "-----------------------------------------------------------------------"
printf '%-8s %-24s %s\n' "STATUS" "CHECK" "DETAIL"
for R in "${ROWS[@]}"; do
  IFS='|' read -r S C D <<<"$R"
  printf '%-8s %-24s %s\n' "$S" "$C" "$D"
done
echo "-----------------------------------------------------------------------"
if [ "$FAIL" -gt 0 ]; then
  echo "VERDICT: BLOCKED ($FAIL failing check(s)). Resolve FAIL rows before migrating."
  exit 2
fi
if [ "$REVIEW" -gt 0 ]; then
  echo "VERDICT: READY with $REVIEW review item(s). A human must acknowledge the REVIEW rows (data loss, quarantined nodes) before proceeding."
else
  echo "VERDICT: READY."
fi
echo "Reminder: this migration is a clean reinstall. ALL health event data will be lost."
exit 0
