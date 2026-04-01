// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package helpers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	MongoDBStatefulSetName = "mongodb"
	MongoDBContainerName   = "mongodb"
	MongoDBDatabase        = "HealthEventsDatabase"
	MongoDBTokenCollection = "ResumeTokens"
)

// GetMongoDBPrimaryPodName returns the name of a running MongoDB pod from the StatefulSet.
// For simplicity, it tries mongodb-0 first (the typical primary in a fresh deployment).
func GetMongoDBPrimaryPodName(
	ctx context.Context, t *testing.T, client klient.Client,
) string {
	t.Helper()

	pods := &v1.PodList{}
	err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.LabelSelector = "app.kubernetes.io/component=mongodb"
	})
	require.NoError(t, err, "failed to list MongoDB pods")

	for _, pod := range pods.Items {
		if pod.Namespace != NVSentinelNamespace {
			continue
		}

		if pod.Status.Phase == v1.PodRunning {
			t.Logf("Found running MongoDB pod: %s", pod.Name)
			return pod.Name
		}
	}

	require.Fail(t, "no running MongoDB pod found in namespace %s", NVSentinelNamespace)

	return ""
}

// buildMongoshCommand constructs a mongosh command that authenticates via
// SCRAM-SHA-256 with TLS against the headless service hostname (which matches
// the TLS certificate SAN), matching the standard manual connection method.
//
// The password is read from the MONGODB_ROOT_PASSWORD env var that the bitnami
// chart injects into the MongoDB pod, so it is never embedded in the command
// string or printed in test logs.
//
// IMPORTANT: All JavaScript passed to jsEval MUST use double quotes for strings
// (not single quotes), because the --eval argument is wrapped in single quotes
// to prevent shell expansion of $ characters (e.g. $set, $external).
func buildMongoshCommand(mongoPod, jsEval string) []string {
	host := fmt.Sprintf("%s.mongodb-headless.%s.svc.cluster.local", mongoPod, NVSentinelNamespace)

	return []string{
		"/bin/sh", "-c",
		fmt.Sprintf(
			`mongosh --quiet `+
				`--host %s `+
				`--tls `+
				`--tlsCAFile /certs/mongodb-ca-cert `+
				`--tlsCertificateKeyFile /certs/mongodb.pem `+
				`--username root `+
				`--password "$MONGODB_ROOT_PASSWORD" `+
				`--authenticationDatabase admin `+
				`--authenticationMechanism SCRAM-SHA-256 `+
				`--eval '%s'`,
			host, jsEval,
		),
	}
}

// ExecMongosh runs a JavaScript expression inside a MongoDB pod via mongosh.
// Returns stdout and stderr from the command execution.
// All JS strings in the eval expression MUST use double quotes (not single quotes).
func ExecMongosh(
	ctx context.Context, t *testing.T, restConfig *rest.Config, mongoPod, js string,
) (string, string) {
	t.Helper()

	cmd := buildMongoshCommand(mongoPod, js)
	stdout, stderr, err := ExecInPod(ctx, restConfig, NVSentinelNamespace, mongoPod, MongoDBContainerName, cmd)
	require.NoError(t, err, "mongosh exec failed: stdout=%s stderr=%s", stdout, stderr)

	return stdout, stderr
}

// StaleTokenMarker is the _data value inserted by InsertStaleResumeToken.
// It is not valid hex, causing MongoDB to return FailedToParse (error code 9) on the
// Watch() call. Tests check for this string to verify the recovery path deleted the token.
const StaleTokenMarker = "INVALID_STALE_TOKEN"

// InsertStaleResumeToken inserts an invalid resume token for the given clientName into
// the ResumeTokens collection. The token's _data is not valid hex, causing MongoDB to
// return FailedToParse (error code 9) on the Watch() call. This works regardless of
// oplog state.
func InsertStaleResumeToken(
	ctx context.Context, t *testing.T, restConfig *rest.Config, mongoPod, clientName string,
) {
	t.Helper()
	t.Logf("Inserting stale resume token for client %q into MongoDB", clientName)

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		db.%s.updateOne(
			{ clientName: "%s" },
			{ $set: { clientName: "%s", resumeToken: { _data: "%s" } } },
			{ upsert: true }
		);
		let saved = db.%s.findOne({ clientName: "%s" });
		printjson(saved);
		print("Stale resume token inserted for %s");
	`, MongoDBDatabase,
		MongoDBTokenCollection, clientName, clientName, StaleTokenMarker,
		MongoDBTokenCollection, clientName, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, mongoPod, js)
	t.Logf("InsertStaleResumeToken output: %s", strings.TrimSpace(stdout))
}

// DeleteResumeToken removes the resume token for the given clientName from MongoDB.
func DeleteResumeToken(
	ctx context.Context, t *testing.T, restConfig *rest.Config, mongoPod, clientName string,
) {
	t.Helper()
	t.Logf("Deleting resume token for client %q from MongoDB", clientName)

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		result = db.%s.deleteOne({ clientName: "%s" });
		print("Deleted " + result.deletedCount + " resume token(s) for %s");
	`, MongoDBDatabase, MongoDBTokenCollection, clientName, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, mongoPod, js)
	t.Logf("DeleteResumeToken output: %s", strings.TrimSpace(stdout))
}

// GetResumeTokenDoc returns the resume token document for the given clientName, or empty string if not found.
func GetResumeTokenDoc(
	ctx context.Context, t *testing.T, restConfig *rest.Config, mongoPod, clientName string,
) string {
	t.Helper()

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		doc = db.%s.findOne({ clientName: "%s" });
		if (doc) { printjson(doc); } else { print("NOT_FOUND"); }
	`, MongoDBDatabase, MongoDBTokenCollection, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, mongoPod, js)

	return strings.TrimSpace(stdout)
}
