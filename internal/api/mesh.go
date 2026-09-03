package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/agentculture/culture-nodes/internal/mesh"
	"github.com/agentculture/culture-nodes/internal/worker"
)

type meshActor struct {
	ActorKey string           `json:"actor_key"`
	Machine  *string          `json:"machine"`
	Bridge   mesh.Observation `json:"bridge"`
}

type meshMachine struct {
	Actors []string `json:"actors"`
}
type meshWorker struct {
	WorkerID  string    `json:"worker_id"`
	Hostname  string    `json:"hostname"`
	Revision  string    `json:"revision"`
	ActorKeys []string  `json:"actor_keys"`
	LastSeen  time.Time `json:"last_seen"`
	Reason    string    `json:"reason,omitempty"`
}
type meshOut struct {
	Actors   []meshActor            `json:"actors"`
	Machines map[string]meshMachine `json:"machines"`
	Version  string                 `json:"version"`
	Workers  []meshWorker           `json:"workers"`
}

type meshActorRow struct {
	key      string
	hostname string
}

func meshActors(ctx context.Context, s *Server) ([]meshActorRow, error) {
	rows, err := s.Store.Pool().Query(ctx, `
SELECT DISTINCT ON (actor_key) actor_key, COALESCE(capabilities #>> '{preflight,host,hostname}', '')
FROM actors WHERE namespace_id = $1 ORDER BY actor_key, revision DESC`, s.NamespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []meshActorRow
	for rows.Next() {
		var row meshActorRow
		if err := rows.Scan(&row.key, &row.hostname); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func meshWorkers(ctx context.Context, s *Server) ([]meshWorker, error) {
	rows, err := s.Store.Pool().Query(ctx, `SELECT worker_id, hostname, revision, actor_keys, last_seen FROM worker_presence ORDER BY worker_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]meshWorker, 0)
	for rows.Next() {
		var row meshWorker
		if err := rows.Scan(&row.WorkerID, &row.Hostname, &row.Revision, &row.ActorKeys, &row.LastSeen); err != nil {
			return nil, err
		}
		row.Reason = worker.HostnameReason(row.Hostname)
		out = append(out, row)
	}
	return out, rows.Err()
}

func buildMesh(actorRows []meshActorRow, workers []meshWorker, version string, observations map[string]mesh.Observation) meshOut {
	out := meshOut{Actors: make([]meshActor, 0, len(actorRows)), Machines: make(map[string]meshMachine), Version: version, Workers: workers}
	for _, row := range actorRows {
		observation, observed := observations[row.key]
		if !observed {
			observation.Class = "unobserved"
			observation.Error = "not observed by the bridge collector"
		}
		actor := meshActor{ActorKey: row.key, Bridge: observation}
		if observation.Hostname != "" {
			hostname := observation.Hostname
			actor.Machine = &hostname
			machine := out.Machines[hostname]
			machine.Actors = append(machine.Actors, row.key)
			out.Machines[hostname] = machine
		}
		out.Actors = append(out.Actors, actor)
	}
	for hostname, machine := range out.Machines {
		sort.Strings(machine.Actors)
		out.Machines[hostname] = machine
	}
	return out
}

func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) error {
	actors, err := meshActors(r.Context(), s)
	if err != nil {
		return internalError(err)
	}
	workers, err := meshWorkers(r.Context(), s)
	if err != nil {
		return internalError(err)
	}
	observations := map[string]mesh.Observation{}
	if s.meshCollector != nil {
		observations = s.meshCollector.Snapshot()
	}
	// Marshal once through writeJSON. encoding/json sorts map keys, keeping the
	// representation deterministic as long as the committed rows are unchanged.
	writeJSON(w, http.StatusOK, buildMesh(actors, workers, s.buildVersion, observations))
	return nil
}
