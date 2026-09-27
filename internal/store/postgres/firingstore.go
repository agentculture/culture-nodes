package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/store"
)

// DeclarationFiring is one physical dispatch of a declaration. Re-mints have
// their own ID, but retain the original CanonicalFiringID and LineageID.
type DeclarationFiring struct {
	ID                 string
	NamespaceID        string
	EventID            string
	DeclarationID      string
	DeclarationVersion string
	TriggerDigest      string
	ConditionDigest    string
	ActionDigest       string
	LineageID          string
	CanonicalFiringID  string
	RemintOfID         string
	CreatedAt          time.Time
}

type DeclarationFiringInput struct {
	NamespaceID        string
	EventID            string
	DeclarationID      string
	DeclarationVersion string
	TriggerDigest      string
	ConditionDigest    string
	ActionDigest       string
	// ParentFiringID links a new logical firing to the event's verified
	// causal predecessor. Empty starts a new lineage.
	ParentFiringID string
	// RemintOfID identifies the prior physical dispatch. A re-mint retains
	// that firing's canonical entry and lineage and creates no lineage edge.
	RemintOfID string
}

const declarationFiringColumns = `id,namespace_id,event_id,declaration_id,declaration_version,
	trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id,
	COALESCE(remint_of_id,''),created_at`

func scanDeclarationFiring(row pgx.Row) (DeclarationFiring, error) {
	var f DeclarationFiring
	err := row.Scan(&f.ID, &f.NamespaceID, &f.EventID, &f.DeclarationID,
		&f.DeclarationVersion, &f.TriggerDigest, &f.ConditionDigest,
		&f.ActionDigest, &f.LineageID, &f.CanonicalFiringID,
		&f.RemintOfID, &f.CreatedAt)
	return f, err
}

// RecordDeclarationFiring inserts a firing once per event and declaration.
// The event ID must already exist in signal_events. DeliverSignalEvent's
// watermark duplicate path returns the same ID, so retries select the row
// created by the first delivery. Re-mints are separate physical rows keyed
// by the prior firing, but have one logical entry for lineage and loop counts.
// The returned bool is true only when this call created a row.
func (s *Store) RecordDeclarationFiring(ctx context.Context, in DeclarationFiringInput) (DeclarationFiring, bool, error) {
	if in.NamespaceID == "" || in.EventID == "" || in.DeclarationID == "" ||
		in.DeclarationVersion == "" || in.TriggerDigest == "" ||
		in.ConditionDigest == "" || in.ActionDigest == "" {
		return DeclarationFiring{}, false, errors.New("postgres: RecordDeclarationFiring: namespace, event, declaration, version and component digests are required")
	}
	if in.ParentFiringID != "" && in.RemintOfID != "" {
		return DeclarationFiring{}, false, errors.New("postgres: RecordDeclarationFiring: parent and re-mint source are mutually exclusive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var eventID string
	if err := tx.QueryRow(ctx, `SELECT id FROM signal_events WHERE namespace_id=$1 AND id=$2`, in.NamespaceID, in.EventID).Scan(&eventID); err != nil {
		return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: event: %w", err)
	}
	id := store.NewULID()
	lineageID, canonicalID := id, id
	parentCanonicalID := ""
	if in.RemintOfID != "" {
		var sourceEventID, sourceDeclarationID string
		err = tx.QueryRow(ctx, `SELECT event_id,declaration_id,lineage_id,canonical_firing_id
			FROM declaration_firings WHERE namespace_id=$1 AND id=$2 FOR UPDATE`,
			in.NamespaceID, in.RemintOfID).Scan(&sourceEventID, &sourceDeclarationID, &lineageID, &canonicalID)
		if err != nil {
			return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: re-mint source: %w", err)
		}
		if sourceEventID != in.EventID || sourceDeclarationID != in.DeclarationID {
			return DeclarationFiring{}, false, errors.New("postgres: RecordDeclarationFiring: re-mint must use the original event and declaration")
		}
	} else if in.ParentFiringID != "" {
		err = tx.QueryRow(ctx, `SELECT lineage_id,canonical_firing_id FROM declaration_firings
			WHERE namespace_id=$1 AND id=$2`, in.NamespaceID, in.ParentFiringID).Scan(&lineageID, &parentCanonicalID)
		if err != nil {
			return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: parent: %w", err)
		}
	}

	const insert = `INSERT INTO declaration_firings
		(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,
		 condition_digest,action_digest,lineage_id,canonical_firing_id,remint_of_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''))
		ON CONFLICT DO NOTHING RETURNING ` + declarationFiringColumns
	f, err := scanDeclarationFiring(tx.QueryRow(ctx, insert, id, in.NamespaceID, in.EventID,
		in.DeclarationID, in.DeclarationVersion, in.TriggerDigest, in.ConditionDigest,
		in.ActionDigest, lineageID, canonicalID, in.RemintOfID))
	created := err == nil
	if err == pgx.ErrNoRows {
		if in.RemintOfID != "" {
			f, err = scanDeclarationFiring(tx.QueryRow(ctx, `SELECT `+declarationFiringColumns+`
				FROM declaration_firings WHERE namespace_id=$1 AND remint_of_id=$2`, in.NamespaceID, in.RemintOfID))
		} else {
			f, err = scanDeclarationFiring(tx.QueryRow(ctx, `SELECT `+declarationFiringColumns+`
				FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 AND remint_of_id IS NULL`,
				in.NamespaceID, in.EventID, in.DeclarationID))
		}
	}
	if err != nil {
		return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: insert or read: %w", err)
	}
	if created && in.ParentFiringID != "" {
		// An edge points to the logical entry, not each retry dispatch.
		_, err = tx.Exec(ctx, `INSERT INTO declaration_lineage_edges(namespace_id,parent_firing_id,child_firing_id)
			VALUES($1,$2,$3)`, in.NamespaceID, parentCanonicalID, f.ID)
		if err != nil {
			return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: lineage edge: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DeclarationFiring{}, false, fmt.Errorf("postgres: RecordDeclarationFiring: commit: %w", err)
	}
	return f, created, nil
}
