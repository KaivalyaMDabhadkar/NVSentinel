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
# migrate-data.sh - optional data preservation for the Bitnami -> Percona migration.
# dump:    streams a mongodump archive out of the Bitnami mongod (excluding ResumeTokens,
#          which are only valid on the cluster that created them).
# restore: streams the archive into the Percona mongod. ObjectIDs are preserved, so node
#          annotations and CR names that reference event IDs stay valid.
#
# Usage:
#   migrate-data.sh dump    <archive-file>     (run while the Bitnami backend is up)
#   migrate-data.sh restore <archive-file>     (run after verify.sh passes on Percona)
#
# Exit codes: 0 = success, 1 = error.
set -uo pipefail

NS="${NVSENTINEL_NAMESPACE:-nvsentinel}"
DB="${NVSENTINEL_DATABASE:-HealthEventsDatabase}"
TOKEN_COLLECTION="${NVSENTINEL_TOKEN_COLLECTION:-ResumeTokens}"

MODE="${1:-}"
ARCHIVE="${2:-}"
if [ -z "$MODE" ] || [ -z "$ARCHIVE" ]; then
  echo "usage: $0 dump|restore <archive-file>" >&2
  exit 1
fi

case "$MODE" in
dump)
  # Guard: writers that create references to events (annotations, remediation CRs)
  # should be stopped before the dump. An event written after the dump is absent
  # from the archive, and a reference created for it would dangle after restore.
  ACTIVE_WRITERS=""
  for D in fault-quarantine node-drainer fault-remediation; do
    R="$(kubectl get deploy "$D" -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    [ -n "$R" ] && [ "$R" != "0" ] && ACTIVE_WRITERS="$ACTIVE_WRITERS $D"
  done
  if [ -n "$ACTIVE_WRITERS" ] && [ "${ALLOW_ACTIVE_WRITERS:-0}" != "1" ]; then
    echo "WARNING: these components are still running and can create references to events" >&2
    echo "written after the dump:$ACTIVE_WRITERS" >&2
    echo "Scale them to 0 first (kubectl scale deploy$ACTIVE_WRITERS --replicas=0 -n $NS)," >&2
    echo "or re-run with ALLOW_ACTIVE_WRITERS=1 to accept the risk." >&2
    if [ -t 0 ]; then
      echo "Type YES to continue anyway:"
      read -r ANSWER
      [ "$ANSWER" = "YES" ] || { echo "aborted."; exit 3; }
    else
      exit 3
    fi
  fi

  # Detect the source backend: Bitnami (statefulset/mongodb) or Percona (psmdb/mongodb).
  # Server certificates on both sides are issued for pod/service FQDNs, never localhost.
  # Credentials are passed to the pod over stdin (never on a command line), which also
  # keeps passwords with shell-significant characters intact.
  if kubectl get statefulset mongodb -n "$NS" >/dev/null 2>&1; then
    PASSWORD="$(kubectl get secret mongodb -n "$NS" -o jsonpath='{.data.mongodb-root-password}' | base64 -d)"
    if [ -z "$PASSWORD" ]; then
      echo "ERROR: could not read the Bitnami root password (secret 'mongodb')." >&2
      exit 1
    fi
    DUMP_HOST="mongodb-0.mongodb-headless.$NS.svc.cluster.local"
    echo "Dumping $DB (excluding $TOKEN_COLLECTION) from Bitnami mongodb-0..."
    printf '%s\n' "$PASSWORD" | kubectl exec -i -n "$NS" mongodb-0 -c mongodb -- bash -c \
      "IFS= read -r MPW; mongodump --host '$DUMP_HOST' --db '$DB' --excludeCollection '$TOKEN_COLLECTION' \
        --username root --password \"\$MPW\" --authenticationDatabase admin \
        --ssl --sslCAFile certs/mongodb-ca-cert --sslPEMKeyFile certs/mongodb.pem \
        --archive --quiet" > "$ARCHIVE"
    RC=$?
  elif kubectl get psmdb mongodb -n "$NS" >/dev/null 2>&1; then
    PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_BACKUP_USER}' | base64 -d)"
    PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_BACKUP_PASSWORD}' | base64 -d)"
    if [ -z "$PU" ] || [ -z "$PP" ]; then
      # Fall back to the database admin user if the backup user is absent.
      PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_USER}' | base64 -d)"
      PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d)"
    fi
    DUMP_HOST="mongodb-rs0-0.mongodb-rs0.$NS.svc.cluster.local"
    echo "Dumping $DB (excluding $TOKEN_COLLECTION) from Percona mongodb-rs0-0..."
    printf '%s\n%s\n' "$PU" "$PP" | kubectl exec -i -n "$NS" mongodb-rs0-0 -c mongod -- sh -c \
      "IFS= read -r MUSER; IFS= read -r MPW; \
       cat /etc/mongodb-ssl-internal/tls.crt /etc/mongodb-ssl-internal/tls.key > /tmp/dump.pem; \
       mongodump --host '$DUMP_HOST' --db '$DB' --excludeCollection '$TOKEN_COLLECTION' \
        --username \"\$MUSER\" --password \"\$MPW\" --authenticationDatabase admin \
        --ssl --sslCAFile /etc/mongodb-ssl-internal/ca.crt --sslPEMKeyFile /tmp/dump.pem \
        --archive --quiet" > "$ARCHIVE"
    RC=$?
  else
    echo "ERROR: no MongoDB backend found in '$NS' (neither statefulset/mongodb nor psmdb/mongodb)." >&2
    exit 1
  fi
  if [ "$RC" -ne 0 ] || [ ! -s "$ARCHIVE" ]; then
    echo "ERROR: dump failed (rc=$RC, archive size $(wc -c < "$ARCHIVE" 2>/dev/null || echo 0) bytes)." >&2
    exit 1
  fi
  echo "Dump complete: $ARCHIVE ($(wc -c < "$ARCHIVE") bytes)."
  echo "Safe to proceed with the migration. Restore AFTER verify.sh passes on Percona."
  ;;
restore)
  if [ ! -s "$ARCHIVE" ]; then
    echo "ERROR: archive '$ARCHIVE' missing or empty." >&2
    exit 1
  fi
  # Percona: databaseAdmin credentials from internal-mongodb-users, internal TLS certs.
  PU="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_USER}' | base64 -d)"
  PP="$(kubectl get secret internal-mongodb-users -n "$NS" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d)"
  if [ -z "$PU" ] || [ -z "$PP" ]; then
    echo "ERROR: could not read Percona credentials (secret 'internal-mongodb-users')." >&2
    exit 1
  fi
  # The internal certificate is issued for the pod/service FQDNs, not localhost.
  # Credentials travel over stdin ahead of the archive bytes (never on a command
  # line): the two 'read' calls consume the credential lines and mongorestore
  # reads the remainder of the stream as the archive.
  RESTORE_HOST="mongodb-rs0-0.mongodb-rs0.$NS.svc.cluster.local"
  echo "Restoring $DB into mongodb-rs0-0 (ObjectIDs preserved)..."
  { printf '%s\n%s\n' "$PU" "$PP"; cat "$ARCHIVE"; } | \
    kubectl exec -i -n "$NS" mongodb-rs0-0 -c mongod -- sh -c \
    "IFS= read -r MUSER; IFS= read -r MPW; \
     cat /etc/mongodb-ssl-internal/tls.crt /etc/mongodb-ssl-internal/tls.key > /tmp/restore.pem; \
     mongorestore --host '$RESTORE_HOST' --nsInclude '$DB.*' \
      --username \"\$MUSER\" --password \"\$MPW\" --authenticationDatabase admin \
      --ssl --sslCAFile /etc/mongodb-ssl-internal/ca.crt --sslPEMKeyFile /tmp/restore.pem \
      --archive --quiet"
  RC=$?
  if [ "$RC" -ne 0 ]; then
    echo "ERROR: restore failed (rc=$RC)." >&2
    exit 1
  fi
  echo "Restore complete."
  echo "Now restart the datastore consumers so their cold-start logic processes the restored"
  echo "events (kubectl rollout restart on the consumer deployments), and expect the event"
  echo "exporter to re-export restored events to its sink (duplicates downstream)."
  ;;
*)
  echo "usage: $0 dump|restore <archive-file>" >&2
  exit 1
  ;;
esac
