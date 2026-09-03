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
		_, _ = w.Write([]byte(`{"preflight":{"host":{"deployment":{"version":"bridge-rev"}}}}`))
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
	if _, err := f.store.Pool().Exec(ctx, `INSERT INTO worker_presence (worker_id, hostname, revision, actor_keys, last_seen) VALUES ($1,'shared-host','rev-a',ARRAY['alpha'],NOW())`, workerID); err != nil {
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
		Version string            `json:"version"`
		Workers []json.RawMessage `json:"workers"`
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
	if machine := got.Machines["shared-host"].Actors; strings.Join(machine, ",") != "alpha,beta" {
		t.Fatalf("shared machine actors = %v", machine)
	}
	for _, actor := range got.Actors {
		if actor.Bridge.Deployment.Version != "bridge-rev" || actor.Bridge.ObservedAt.IsZero() {
			t.Fatalf("actor %s bridge observation = %+v", actor.ActorKey, actor.Bridge)
		}
		if actor.ActorKey == "nameless" && actor.Machine != nil {
			t.Fatalf("nameless actor machine = %q, want null", *actor.Machine)
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
