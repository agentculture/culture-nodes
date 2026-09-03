package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/mesh"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestMeshDedupesActorsKeysMachinesFromReportedHostnameAndIncludesWorkers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	insert := func(key string, revision int, hostname string) {
		t.Helper()
		capabilities := `{"preflight":{"host":{}}}`
		if hostname != "" {
			capabilities = `{"preflight":{"host":{"hostname":"` + hostname + `"}}}`
		}
		if _, err := f.store.Pool().Exec(ctx, `INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, capabilities) VALUES ($1,$2,$3,$4,'agent','http',$5)`, store.NewULID(), f.nsID, key, revision, capabilities); err != nil {
			t.Fatal(err)
		}
	}
	insert("alpha", 1, "old-host")
	insert("alpha", 2, "shared-host")
	insert("beta", 1, "shared-host")
	insert("nameless", 1, "")
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"shared-host","deployment":{"version":"bridge-rev"}}}}`))
	}))
	defer bridge.Close()
	collector := mesh.New(mesh.Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 2})
	collector.SetTargets([]mesh.Target{
		{Key: "alpha", URL: bridge.URL},
		{Key: "beta", URL: bridge.URL},
		{Key: "nameless", URL: bridge.URL},
	})
	collector.Collect(ctx)
	workerID := "worker-" + store.NewULID()
	if _, err := f.store.Pool().Exec(ctx, `INSERT INTO worker_presence (namespace_id, worker_id, hostname, revision, actor_keys, last_seen) VALUES ($1,$2,'shared-host','rev-a',ARRAY['alpha'],NOW())`, f.nsID, workerID); err != nil {
		t.Fatal(err)
	}
	// A worker of ANOTHER namespace, carrying the same worker id: presence is
	// namespace-scoped state, so this row must be invisible here and must not
	// have overwritten the row above (PR #292 review).
	otherNS := pgtest.MustNamespace(t, f.store, "mesh-other").ID
	if _, err := f.store.Pool().Exec(ctx, `INSERT INTO worker_presence (namespace_id, worker_id, hostname, revision, actor_keys, last_seen) VALUES ($1,$2,'other-host','rev-b',ARRAY['omega'],NOW())`, otherNS, workerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.Pool().Exec(context.Background(), `DELETE FROM worker_presence WHERE worker_id=$1`, workerID)
	})

	srv, err := api.NewServer(f.store, f.nsID, api.WithBuildInfo("9.8.7", "rev"), api.WithMeshCollector(collector))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/v1alpha1/mesh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Actors []struct {
			ID       string  `json:"id"`
			ActorKey string  `json:"actor_key"`
			Machine  *string `json:"machine"`
			Bridge   struct {
				Deployment struct {
					Version string `json:"version"`
				} `json:"deployment"`
				ObservedAt time.Time `json:"observed_at"`
			} `json:"bridge"`
		} `json:"actors"`
		Machines map[string]struct {
			Actors []string `json:"actors"`
		} `json:"machines"`
		Version string `json:"version"`
		Workers []struct {
			WorkerID string `json:"worker_id"`
			Hostname string `json:"hostname"`
			Revision string `json:"revision"`
		} `json:"workers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Actors) != 3 {
		t.Fatalf("actors = %d, want 3 deduped identities", len(got.Actors))
	}
	if got.Version != "9.8.7" || len(got.Workers) != 1 {
		t.Fatalf("version/workers = %q/%d", got.Version, len(got.Workers))
	}
	if got.Workers[0].Hostname != "shared-host" || got.Workers[0].Revision != "rev-a" {
		t.Fatalf("worker row = %+v, want this namespace's own presence, not another namespace's", got.Workers[0])
	}
	if machine := got.Machines["shared-host"].Actors; strings.Join(machine, ",") != "alpha,beta,nameless" {
		t.Fatalf("shared machine actors = %v", machine)
	}
	for _, actor := range got.Actors {
		if actor.Bridge.Deployment.Version != "bridge-rev" || actor.Bridge.ObservedAt.IsZero() {
			t.Fatalf("actor %s bridge observation = %+v", actor.ActorKey, actor.Bridge)
		}
		if actor.Machine == nil || *actor.Machine != "shared-host" {
			t.Fatalf("actor %s machine = %v, want bridge-reported shared-host", actor.ActorKey, actor.Machine)
		}
		// The actors-table row id of the CURRENT revision: the identity
		// attempts.actor_id records, and so the only value a reader can join
		// GET /v1alpha1/node-runs attribution on (PR #292 review).
		if actor.ID == "" {
			t.Fatalf("actor %s has no row id; run attribution cannot be joined to it", actor.ActorKey)
		}
	}
}

func TestMeshNeverProbesInRequestAndEndpointRefCannotAffectPayload(t *testing.T) {
	f := newFixture(t)
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	c := mesh.New(mesh.Config{Interval: time.Hour, ProbeTimeout: 15 * time.Millisecond, MaxConcurrency: 1})
	c.SetTargets([]mesh.Target{{Key: "slow", URL: blocked.URL}})
	c.Collect(context.Background()) // cache the timeout before serving reads
	_, err := f.store.Pool().Exec(context.Background(), `INSERT INTO actors (id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref,capabilities) VALUES ($1,$2,'slow',1,'agent','http',NULL,'{"preflight":{"host":{"hostname":"host-a"}}}')`, store.NewULID(), f.nsID)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(f.store, f.nsID, api.WithMeshCollector(c))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	read := func() []byte {
		t.Helper()
		started := time.Now()
		resp, err := ts.Client().Get(ts.URL + "/v1alpha1/mesh")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if time.Since(started) > 100*time.Millisecond {
			t.Fatalf("mesh read waited for a probe")
		}
		return body
	}
	before := read()
	if !strings.Contains(string(before), `"observed_at"`) || !strings.Contains(string(before), `"error"`) {
		t.Fatalf("cached failure absent: %s", before)
	}
	if _, err := f.store.Pool().Exec(context.Background(), `UPDATE actors SET endpoint_ref = $1 WHERE namespace_id=$2 AND actor_key='slow'`, blocked.URL, f.nsID); err != nil {
		t.Fatal(err)
	}
	after := read()
	if string(before) != string(after) {
		t.Fatalf("endpoint_ref changed mesh bytes\nbefore=%s\nafter=%s", before, after)
	}
}
