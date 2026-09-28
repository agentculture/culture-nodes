package api

import (
	"context"
	"fmt"
	"time"
)

// FiringOut names the immutable declaration version and causal lineage of an
// execution run. Graph runs have no firing, so the field is omitted for them.
type FiringOut struct {
	ID                 string       `json:"id"`
	LineageID          string       `json:"lineage_id"`
	CanonicalFiringID  string       `json:"canonical_firing_id"`
	EventID            string       `json:"event_id"`
	DeclarationID      string       `json:"declaration_id"`
	DeclarationName    string       `json:"declaration_name"`
	DeclarationVersion string       `json:"declaration_version"`
	VersionDigest      string       `json:"version_digest"`
	TriggerDigest      string       `json:"trigger_digest"`
	ConditionDigest    string       `json:"condition_digest"`
	ActionDigest       string       `json:"action_digest"`
	CreatedAt          time.Time    `json:"created_at"`
	NodeRuns           []NodeRunOut `json:"node_runs,omitempty"`
}

// firingsForRuns is a single namespace-scoped read for any page of run IDs.
// Version names come from the pinned version, not the current alias head.
func (s *Server) firingsForRuns(ctx context.Context, ids []string) (map[string]FiringOut, error) {
	out := make(map[string]FiringOut)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Store.Pool().Query(ctx, `SELECT f.id,f.lineage_id,f.canonical_firing_id,f.event_id,
		f.declaration_id,d.name,f.declaration_version,v.digest,f.trigger_digest,f.condition_digest,
		f.action_digest,f.created_at FROM declaration_firings f
		JOIN declaration_versions v ON v.namespace_id=f.namespace_id AND v.id=f.declaration_version
		JOIN declarations d ON d.namespace_id=f.namespace_id AND d.id=v.declaration_id
		WHERE f.namespace_id=$1 AND f.id=ANY($2)`, s.NamespaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("api: firing read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f FiringOut
		if err := rows.Scan(&f.ID, &f.LineageID, &f.CanonicalFiringID, &f.EventID,
			&f.DeclarationID, &f.DeclarationName, &f.DeclarationVersion, &f.VersionDigest, &f.TriggerDigest,
			&f.ConditionDigest, &f.ActionDigest, &f.CreatedAt); err != nil {
			return nil, err
		}
		out[f.ID] = f
	}
	return out, rows.Err()
}

func (s *Server) lineageFirings(ctx context.Context, firing FiringOut) ([]FiringOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `SELECT f.id,f.lineage_id,f.canonical_firing_id,f.event_id,
		f.declaration_id,d.name,f.declaration_version,v.digest,f.trigger_digest,f.condition_digest,
		f.action_digest,f.created_at FROM declaration_firings f
		JOIN declaration_versions v ON v.namespace_id=f.namespace_id AND v.id=f.declaration_version
		JOIN declarations d ON d.namespace_id=f.namespace_id AND d.id=v.declaration_id
		WHERE f.namespace_id=$1 AND f.lineage_id=$2 ORDER BY f.created_at,f.id`, s.NamespaceID, firing.LineageID)
	if err != nil {
		return nil, err
	}
	var out []FiringOut
	for rows.Next() {
		var f FiringOut
		if err := rows.Scan(&f.ID, &f.LineageID, &f.CanonicalFiringID, &f.EventID,
			&f.DeclarationID, &f.DeclarationName, &f.DeclarationVersion, &f.VersionDigest, &f.TriggerDigest,
			&f.ConditionDigest, &f.ActionDigest, &f.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].NodeRuns, err = s.runNodeRuns(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) enrichRuns(ctx context.Context, runs []RunOut) error {
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = runs[i].ID
	}
	byID, err := s.firingsForRuns(ctx, ids)
	if err != nil {
		return err
	}
	for i := range runs {
		if f, ok := byID[runs[i].ID]; ok {
			runs[i].Firing = &f
		}
	}
	return nil
}

func (s *Server) enrichHumanTasks(ctx context.Context, tasks []HumanTaskOut) error {
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].RunID
	}
	byID, err := s.firingsForRuns(ctx, ids)
	if err != nil {
		return err
	}
	for i := range tasks {
		if f, ok := byID[tasks[i].RunID]; ok {
			tasks[i].Firing = &f
		}
	}
	return s.resolveHumanTaskContexts(ctx, tasks)
}

func (s *Server) enrichPendingDecisions(ctx context.Context, groups []PendingDecisionRunOut) error {
	ids := make([]string, len(groups))
	for i := range groups {
		ids[i] = groups[i].RunID
	}
	byID, err := s.firingsForRuns(ctx, ids)
	if err != nil {
		return err
	}
	for i := range groups {
		if f, ok := byID[groups[i].RunID]; ok {
			groups[i].Firing = &f
		}
	}
	return nil
}
