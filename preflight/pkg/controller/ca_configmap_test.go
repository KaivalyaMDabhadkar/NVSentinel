// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	caTestName = "nvsentinel-platform-connector-ca"
	caOldPEM   = "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n"
	caNewPEM   = "-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----\n"
)

func writeCAFile(t *testing.T, dir, pem string) string {
	t.Helper()

	path := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(path, []byte(pem), 0o600))

	return path
}

// caCopy is a ConfigMap copy as the sync would have written it.
func caCopy(namespace, pem string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		Name:      caTestName,
		Namespace: namespace,
		Labels:    caBundleLabels(),
		Data:      map[string]string{caBundleKey: pem},
	}
}

func getCM(t *testing.T, c client.Client, namespace, name string) *corev1.ConfigMap {
	t.Helper()

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, cm))

	return cm
}

func TestCABundleSync_Ensure(t *testing.T) {
	t.Run("creates the copy with the bundle and labels", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caOldPEM), caTestName)

		require.NoError(t, s.Ensure(context.Background(), "team-a"))

		cm := getCM(t, c, "team-a", caTestName)
		assert.Equal(t, caOldPEM, cm.Data[caBundleKey])
		assert.Equal(t, "preflight", cm.Labels[caBundleManagedByLabel])
		assert.Equal(t, "true", cm.Labels[caBundleLabel])
	})

	t.Run("no-op when the copy already matches", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caOldPEM), caTestName)

		before := getCM(t, c, "team-a", caTestName).ResourceVersion

		require.NoError(t, s.Ensure(context.Background(), "team-a"))

		assert.Equal(t, before, getCM(t, c, "team-a", caTestName).ResourceVersion, "an equal copy must not be written")
	})

	t.Run("updates the copy when the file changed", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		dir := t.TempDir()
		path := writeCAFile(t, dir, caOldPEM)
		s := NewCABundleSync(c, c, path, caTestName)

		require.NoError(t, s.Ensure(context.Background(), "team-a"))
		require.Equal(t, caOldPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey])

		writeCAFile(t, dir, caNewPEM)

		require.NoError(t, s.Ensure(context.Background(), "team-a"))

		cm := getCM(t, c, "team-a", caTestName)
		assert.Equal(t, caNewPEM, cm.Data[caBundleKey])
		assert.Equal(t, "true", cm.Labels[caBundleLabel])
	})

	t.Run("AlreadyExists on create is treated as a re-read", func(t *testing.T) {
		// The reader misses the copy once, so Create hits AlreadyExists; the
		// sync must then read the real copy and update it.
		base := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		missedOnce := false
		reader := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if !missedOnce {
					missedOnce = true

					return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
				}

				return c.Get(ctx, key, obj, opts...)
			},
		})
		s := NewCABundleSync(base, reader, writeCAFile(t, t.TempDir(), caNewPEM), caTestName)

		require.NoError(t, s.Ensure(context.Background(), "team-a"))

		assert.True(t, missedOnce)
		assert.Equal(t, caNewPEM, getCM(t, base, "team-a", caTestName).Data[caBundleKey])
	})

	t.Run("missing CA file is an error", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		s := NewCABundleSync(c, c, filepath.Join(t.TempDir(), "missing.crt"), caTestName)

		err := s.Ensure(context.Background(), "team-a")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read")
	})

	t.Run("empty CA file is an error", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), "\n"), caTestName)

		err := s.Ensure(context.Background(), "team-a")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})
}

func TestCABundleSync_Refresh(t *testing.T) {
	t.Run("updates every labelled copy and leaves other ConfigMaps alone", func(t *testing.T) {
		unlabelledSameName := &corev1.ConfigMap{
			Name:      caTestName,
			Namespace: "team-c",
			Data:      map[string]string{caBundleKey: caOldPEM},
		}
		other := &corev1.ConfigMap{
			Name:      "other",
			Namespace: "team-a",
			Labels:    map[string]string{caBundleManagedByLabel: "preflight"},
			Data:      map[string]string{caBundleKey: caOldPEM},
		}
		c := fake.NewClientBuilder().WithObjects(
			caCopy("team-a", caOldPEM),
			caCopy("team-b", caOldPEM),
			caCopy("team-current", caNewPEM),
			unlabelledSameName,
			other,
		).Build()

		currentRV := getCM(t, c, "team-current", caTestName).ResourceVersion
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caNewPEM), caTestName)

		s.refresh(context.Background())

		assert.Equal(t, caNewPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey])
		assert.Equal(t, caNewPEM, getCM(t, c, "team-b", caTestName).Data[caBundleKey])
		assert.Equal(t, currentRV, getCM(t, c, "team-current", caTestName).ResourceVersion,
			"a copy that already matches must not be written")
		assert.Equal(t, caOldPEM, getCM(t, c, "team-c", caTestName).Data[caBundleKey],
			"an unlabelled ConfigMap is not ours")
		assert.Equal(t, caOldPEM, getCM(t, c, "team-a", "other").Data[caBundleKey],
			"a ConfigMap without the CA label is not ours")
		assert.Equal(t, []byte(caNewPEM), s.last)
	})

	t.Run("unchanged file does nothing", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caNewPEM), caTestName)
		s.last = []byte(caNewPEM)

		s.refresh(context.Background())

		assert.Equal(t, caOldPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey],
			"no sweep when the bytes did not change since the last tick")
	})

	t.Run("unreadable file keeps the copies and retries next tick", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		s := NewCABundleSync(c, c, filepath.Join(t.TempDir(), "missing.crt"), caTestName)

		s.refresh(context.Background())

		assert.Equal(t, caOldPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey])
		assert.Nil(t, s.last)
	})

	t.Run("first tick sweeps at startup, later ticks only on change", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		dir := t.TempDir()
		s := NewCABundleSync(c, c, writeCAFile(t, dir, caNewPEM), caTestName)

		require.Nil(t, s.last, "last is nil before the first successful read")

		// First tick: last is nil, so the startup sweep runs even though the
		// file did not change since the process started.
		s.tick(context.Background())

		assert.Equal(t, caNewPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey])
		assert.Equal(t, []byte(caNewPEM), s.last)

		// Put the copy behind by hand; the file is unchanged so the next tick
		// must not sweep.
		stale := getCM(t, c, "team-a", caTestName)
		stale.Data[caBundleKey] = caOldPEM
		require.NoError(t, c.Update(context.Background(), stale))

		s.tick(context.Background())

		assert.Equal(t, caOldPEM, getCM(t, c, "team-a", caTestName).Data[caBundleKey],
			"no sweep when the bytes did not change since the last tick")

		// A real change is swept on the following tick.
		writeCAFile(t, dir, caOldPEM+"rotated\n")

		s.tick(context.Background())

		assert.Equal(t, caOldPEM+"rotated\n", getCM(t, c, "team-a", caTestName).Data[caBundleKey])
	})

	t.Run("a failed update keeps last unchanged and still updates the other copies", func(t *testing.T) {
		base := fake.NewClientBuilder().WithObjects(
			caCopy("team-a", caOldPEM),
			caCopy("team-b", caOldPEM),
			caCopy("team-c", caOldPEM),
		).Build()
		c := interceptor.NewClient(base, interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				opts ...client.UpdateOption,
			) error {
				if obj.GetNamespace() == "team-b" {
					return apierrors.NewInternalError(errors.New("etcd hiccup"))
				}

				return cl.Update(ctx, obj, opts...)
			},
		})
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caNewPEM), caTestName)

		s.refresh(context.Background())

		assert.Equal(t, caNewPEM, getCM(t, base, "team-a", caTestName).Data[caBundleKey])
		assert.Equal(t, caOldPEM, getCM(t, base, "team-b", caTestName).Data[caBundleKey])
		assert.Equal(t, caNewPEM, getCM(t, base, "team-c", caTestName).Data[caBundleKey],
			"a failure in one namespace must not stop the sweep")
		assert.Nil(t, s.last, "last must not advance while a copy is behind, so the next tick retries")

		// Once the update goes through the sweep completes and last advances.
		s2 := NewCABundleSync(base, base, s.caFile, caTestName)
		s2.refresh(context.Background())

		assert.Equal(t, caNewPEM, getCM(t, base, "team-b", caTestName).Data[caBundleKey])
		assert.Equal(t, []byte(caNewPEM), s2.last)
	})

	t.Run("Start returns when the context is done", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caOldPEM), caTestName)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		require.NoError(t, s.Start(ctx))
	})
}

func TestCABundleSync_Pending(t *testing.T) {
	t.Run("a namespace whose Create failed is repaired on the next tick", func(t *testing.T) {
		base := fake.NewClientBuilder().Build()
		failCreate := true
		c := interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				opts ...client.CreateOption,
			) error {
				if failCreate {
					return apierrors.NewInternalError(errors.New("etcd hiccup"))
				}

				return cl.Create(ctx, obj, opts...)
			},
		})
		s := NewCABundleSync(c, c, writeCAFile(t, t.TempDir(), caOldPEM), caTestName)

		err := s.Ensure(context.Background(), "team-a")
		require.Error(t, err)
		assert.Equal(t, []string{"team-a"}, s.pendingNamespaces(), "a failed Ensure records the namespace")

		// The API server keeps failing: the namespace stays pending.
		s.tick(context.Background())

		assert.Equal(t, []string{"team-a"}, s.pendingNamespaces())

		failCreate = false

		s.tick(context.Background())

		assert.Equal(t, caOldPEM, getCM(t, base, "team-a", caTestName).Data[caBundleKey])
		assert.Empty(t, s.pendingNamespaces(), "a repaired namespace leaves the set")
	})

	t.Run("a namespace whose Get failed is repaired on the next tick", func(t *testing.T) {
		base := fake.NewClientBuilder().WithObjects(caCopy("team-a", caOldPEM)).Build()
		failGet := true
		reader := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if failGet {
					return apierrors.NewInternalError(errors.New("etcd hiccup"))
				}

				return cl.Get(ctx, key, obj, opts...)
			},
		})
		s := NewCABundleSync(base, reader, writeCAFile(t, t.TempDir(), caNewPEM), caTestName)

		require.Error(t, s.Ensure(context.Background(), "team-a"))
		assert.Equal(t, []string{"team-a"}, s.pendingNamespaces())

		failGet = false

		s.tick(context.Background())

		assert.Equal(t, caNewPEM, getCM(t, base, "team-a", caTestName).Data[caBundleKey])
		assert.Empty(t, s.pendingNamespaces())
	})

	t.Run("a successful Ensure clears the namespace even outside the tick", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		dir := t.TempDir()
		// The CA file is not there yet, so both Ensure calls fail.
		s := NewCABundleSync(c, c, filepath.Join(dir, "ca.crt"), caTestName)

		require.Error(t, s.Ensure(context.Background(), "team-a"))
		require.Error(t, s.Ensure(context.Background(), "team-b"))
		assert.Equal(t, []string{"team-a", "team-b"}, s.pendingNamespaces())

		writeCAFile(t, dir, caOldPEM)

		// The webhook calls Ensure again for team-a before the tick fires.
		require.NoError(t, s.Ensure(context.Background(), "team-a"))
		assert.Equal(t, []string{"team-b"}, s.pendingNamespaces())

		s.tick(context.Background())

		assert.Empty(t, s.pendingNamespaces())
		assert.Equal(t, caOldPEM, getCM(t, c, "team-b", caTestName).Data[caBundleKey])
	})
}

// The reconciler runs under a manager against a real API server here: the
// copy goes through the same uncached read and Create as in production.
func TestNamespaceReconciler_EnsuresCACopyForActiveNamespaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	active, te := setupNSTestEnv(t, ctx, writeCAFile(t, t.TempDir(), caOldPEM))
	defer te.teardown()

	_, err := te.kubeClient.CoreV1().Namespaces().Create(ctx, preflightNamespace("team-a"), metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = te.kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{Name: "team-b"}, metav1.CreateOptions{})
	require.NoError(t, err)

	var cm *corev1.ConfigMap

	require.Eventually(t, func() bool {
		cm, err = te.kubeClient.CoreV1().ConfigMaps("team-a").Get(ctx, caTestName, metav1.GetOptions{})
		return err == nil
	}, 10*time.Second, 100*time.Millisecond, "the labelled namespace gets a copy of the CA bundle")
	assert.True(t, active.Contains("team-a"))
	assert.Equal(t, caOldPEM, cm.Data[caBundleKey])
	assert.Equal(t, caBundleLabels(), cm.Labels)

	require.Never(t, func() bool {
		_, err := te.kubeClient.CoreV1().ConfigMaps("team-b").Get(ctx, caTestName, metav1.GetOptions{})
		return err == nil
	}, 2*time.Second, 100*time.Millisecond, "an unlabelled namespace gets no copy from the reconciler")
	assert.False(t, active.Contains("team-b"))
}
