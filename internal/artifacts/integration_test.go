package artifacts_test

// This file (plus podtestmain_test.go) is the pod-agnostic proof task t15
// asks for: two separate Store instances, each with its own fresh
// connections, sharing one Postgres and one MinIO instance -- standing in
// for two replicas of the same Deployment -- prove an artifact written
// through one is byte-identical and digest-verified when read through the
// other.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	artifactpg "github.com/agentculture/culture-nodes/internal/artifacts/postgres"
	artifacts3 "github.com/agentculture/culture-nodes/internal/artifacts/s3"
	"github.com/agentculture/culture-nodes/internal/store"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// routerThreshold is deliberately small so both the small (Postgres) and
// object (MinIO) paths are exercised by short test payloads.
const routerThreshold = 64

// pod is one simulated replica: its own Postgres connection pool and its
// own MinIO client, wired into a Router. Two pods constructed against the
// same dbURL/bucket share the same backing services but nothing else --
// exactly the "fresh connections, simulating two pods" the task asks for.
type pod struct {
	pg     *pgstore.Store
	router *artifacts.Router
}

func newPod(t *testing.T, bucket string) *pod {
	t.Helper()
	ctx := context.Background()

	pg, err := pgstore.Connect(ctx, testDBURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)

	small := artifactpg.New(pg, artifactpg.DefaultCapBytes)

	object, err := artifacts3.New(ctx, artifacts3.Config{
		Endpoint:  testMinIOEndpoint,
		AccessKey: testMinIOAccessKey,
		SecretKey: testMinIOSecretKey,
		Bucket:    bucket,
		UseTLS:    false,
	}, pg)
	if err != nil {
		t.Fatalf("s3 driver: %v", err)
	}

	return &pod{pg: pg, router: artifacts.NewRouter(small, object, routerThreshold)}
}

func TestPodAgnosticArtifactStore(t *testing.T) {
	requireBackends(t)
	ctx := context.Background()

	bucket := "nodes-artifacts-it-" + strings.ToLower(store.NewULID())
	podA := newPod(t, bucket)
	podB := newPod(t, bucket) // fresh connections to the SAME Postgres + MinIO

	ns, err := podA.pg.CreateNamespace(ctx, "artifacts-it-"+store.NewULID(), "Artifacts Integration Test")
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	cases := []struct {
		name    string
		content []byte
	}{
		{"small-goes-to-postgres", []byte("hello from pod A -- this is small")},           // < routerThreshold
		{"large-goes-to-minio", bytes.Repeat([]byte("pod-agnostic-artifact-store-"), 20)}, // > routerThreshold
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := podA.router.Put(ctx, artifacts.ArtifactMeta{
				NamespaceID: ns.ID,
				Name:        tc.name,
				MediaType:   "application/octet-stream",
			}, bytes.NewReader(tc.content))
			if err != nil {
				t.Fatalf("Put via pod A: %v", err)
			}

			// Confirm which backend actually received it, so this test is
			// honest about exercising both drivers rather than assuming
			// the threshold math worked.
			meta, err := podB.router.Stat(ctx, ref)
			if err != nil {
				t.Fatalf("Stat via pod B: %v", err)
			}
			wantBackend := artifacts.BackendPostgres
			if int64(len(tc.content)) > routerThreshold {
				wantBackend = artifacts.BackendS3
			}
			if meta.Backend != wantBackend {
				t.Fatalf("Backend = %q, want %q (payload was %d bytes, threshold %d)", meta.Backend, wantBackend, len(tc.content), routerThreshold)
			}

			rc, getMeta, err := podB.router.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get via pod B: %v", err)
			}
			defer rc.Close()

			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read via pod B: %v", err)
			}
			if !bytes.Equal(got, tc.content) {
				t.Fatalf("content read via pod B does not match what pod A wrote: got %d bytes, want %d bytes", len(got), len(tc.content))
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			sum := sha256.Sum256(tc.content)
			wantDigest := artifacts.DigestPrefix + hex.EncodeToString(sum[:])
			if getMeta.Digest != wantDigest {
				t.Fatalf("Digest = %q, want %q", getMeta.Digest, wantDigest)
			}
			if getMeta.SizeBytes != int64(len(tc.content)) {
				t.Fatalf("SizeBytes = %d, want %d", getMeta.SizeBytes, len(tc.content))
			}

			// Reap via pod B must be visible to pod A too -- one shared
			// authority, not two independent ones that happen to agree
			// right now.
			if _, err := podB.router.Reap(ctx, ref, "test/retention", time.Now().UTC()); err != nil {
				t.Fatalf("Reap via pod B: %v", err)
			}
			if _, _, err := podA.router.Get(ctx, ref); !errors.Is(err, artifacts.ErrReaped) {
				t.Fatalf("Get via pod A after reap via pod B = %v, want ErrReaped", err)
			}
		})
	}
}
