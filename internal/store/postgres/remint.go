package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

const (
	RemintMaxAttempts = 2
	RemintWindow      = 24 * time.Hour
	RemintBackoff     = time.Minute

	// RemintSchedulerActorID is the default producer identity the derived
	// decision record of a minted re-mint is written under. It names the
	// SCHEDULER, not the process that ran it — the same argument
	// api.DefaultRepairRouterActorID makes. Like that identity it must be
	// REGISTERED: ledger_records.origin_actor_id is a foreign key to
	// actors(id), so a deployment that never registered it gets that said
	// to it on every due re-mint rather than silently recording none.
	RemintSchedulerActorID = "engine_remint_scheduler"
)

// ScheduleRunRemint records the next bounded re-mint for a technically
// failed trigger-created run. The (subject, failures-in-window) decision here
// is deliberately the shared admission/counting extension point for #202:
// its failure breaker can consult this key and counter before both this path
// and a fresh TriggerEvent mint, without creating another insert path.
func (s *Store) ScheduleRunRemint(ctx context.Context, namespaceID, runID, nodeRunID string, status engine.TechStatus, outcome string, now time.Time) error {
	if !engine.RemintableTechnicalFailure(status, outcome) {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	es, err := NewEngineStore(s, namespaceID)
	if err != nil {
		return err
	}
	return es.InTx(ctx, func(ctx context.Context, etx engine.Tx) error {
		tx := etx.(*engineTx)
		var eventID, digest, subject string
		err := tx.q.QueryRow(ctx, `
			SELECT e.data->>'trigger_event_id', wv.content_digest, COALESCE(r.subject, '')
			FROM runs r JOIN workflow_versions wv ON wv.id=r.workflow_version_id
			JOIN LATERAL (
				SELECT data FROM events WHERE aggregate_id=r.id AND event_type=$3
				ORDER BY sequence ASC LIMIT 1
			) e ON true
			WHERE r.namespace_id=$1 AND r.id=$2 AND NULLIF(e.data->>'trigger_event_id','') IS NOT NULL`,
			namespaceID, runID, engine.TypeRunCreated).Scan(&eventID, &digest, &subject)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil
			} // manual runs never re-mint
			return fmt.Errorf("postgres: schedule run re-mint: provenance: %w", err)
		}
		if _, err := tx.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "trigger-remint:"+namespaceID+":"+eventID); err != nil {
			return err
		}
		var count int
		var windowStart time.Time
		err = tx.q.QueryRow(ctx, `SELECT COUNT(*)::int, COALESCE(MIN(window_started_at), $3)
			FROM trigger_remints WHERE namespace_id=$1 AND original_event_id=$2 AND window_started_at > $3 - make_interval(secs => $4)`,
			namespaceID, eventID, now, RemintWindow.Seconds()).Scan(&count, &windowStart)
		if err != nil {
			return fmt.Errorf("postgres: schedule run re-mint: count: %w", err)
		}
		if count >= RemintMaxAttempts {
			request, _ := json.Marshal(map[string]any{"reason": "trigger re-mint attempts exhausted", "original_event_id": eventID, "attempts": count, "window_seconds": int(RemintWindow.Seconds()), "subject": subject})
			_, err = etx.InsertHumanTask(ctx, engine.HumanTask{RunID: runID, NodeRunID: nodeRunID, Kind: "trigger_remint_exhausted", Status: engine.HumanTaskStatusPending, Request: request, CreatedAt: now})
			return err
		}
		if count == 0 {
			windowStart = now
		}
		_, err = tx.q.Exec(ctx, `INSERT INTO trigger_remints
			(id,namespace_id,source_run_id,original_event_id,workflow_digest,subject,attempt,window_started_at,available_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9) ON CONFLICT (source_run_id) DO NOTHING`,
			store.NewULID(), namespaceID, runID, eventID, digest, subject, count+1, windowStart, now.Add(RemintBackoff))
		if err != nil {
			return fmt.Errorf("postgres: schedule run re-mint: insert: %w", err)
		}
		return nil
	})
}

// EnqueueDueRemints admits due re-mints through Engine.TriggerEvent. That is
// the identical inbound path used by delivered events and it terminates in
// dispatchNode -> Tx.EnqueueWork; there is no re-mint-only work insertion.
// A subject already active leaves the request pending for a later tick.
//
// producerActorID is the registered identity the derived decision record is
// written under; empty selects RemintSchedulerActorID.
func (s *Store) EnqueueDueRemints(ctx context.Context, namespaceID string, runner engine.EventTriggerRunner, producerActorID string, now time.Time) (int, error) {
	if runner == nil {
		return 0, nil
	}
	if producerActorID == "" {
		producerActorID = RemintSchedulerActorID
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	es, err := NewEngineStore(s, namespaceID)
	if err != nil {
		return 0, err
	}
	admitted := 0
	err = es.InTx(ctx, func(ctx context.Context, etx engine.Tx) error {
		tx := etx.(*engineTx)
		rows, err := tx.q.Query(ctx, `SELECT tr.id,tr.source_run_id,tr.original_event_id,tr.workflow_digest,COALESCE(tr.subject,''),tr.attempt,
			wv.workflow_key,wv.source_format,wv.source,wv.normalized_ir,se.name,se.emitter,se.payload
			FROM trigger_remints tr JOIN workflow_versions wv ON wv.namespace_id=tr.namespace_id AND wv.content_digest=tr.workflow_digest
			JOIN signal_events se ON se.id=tr.original_event_id
			WHERE tr.namespace_id=$1 AND tr.status='pending' AND tr.available_at <= $2
			ORDER BY tr.available_at,tr.id FOR UPDATE OF tr SKIP LOCKED LIMIT 16`, namespaceID, now)
		if err != nil {
			return err
		}
		type due struct {
			id, sourceRunID, eventID, digest, subject string
			attempt                                   int
			workflowKey, format, source               string
			ir                                        []byte
			name, emitter                             string
			payload                                   []byte
		}
		var items []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.sourceRunID, &d.eventID, &d.digest, &d.subject, &d.attempt, &d.workflowKey, &d.format, &d.source, &d.ir, &d.name, &d.emitter, &d.payload); err != nil {
				return err
			}
			items = append(items, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range items {
			// Explicit pre-admission check keeps a re-mint pending instead of
			// letting TriggerEvent attach it to the active run.
			if d.subject != "" {
				if err := etx.Lock(ctx, "trigger-subject:"+namespaceID+":"+d.workflowKey+":"+d.subject); err != nil {
					return err
				}
				if _, found, err := etx.ActiveRunBySubject(ctx, d.workflowKey, d.subject); err != nil {
					return err
				} else if found {
					continue
				}
			}
			created, err := runner.TriggerEvent(ctx, etx, engine.TriggerWorkflow{Digest: d.digest, SourceFormat: d.format, Source: d.source, IR: d.ir}, engine.PickupEvent{ID: d.eventID, Name: d.name, Emitter: d.emitter, Payload: d.payload, Subject: d.subject})
			if err != nil {
				return err
			}
			if len(created) != 1 || created[0].RunID == "" || created[0].Attached || created[0].Deferred {
				return fmt.Errorf("postgres: enqueue due re-mint %s: trigger admission returned %+v", d.id, created)
			}
			newRunID := created[0].RunID
			data, _ := json.Marshal(map[string]any{"kind": "trigger_remint", "original_event_id": d.eventID, "attempt": d.attempt, "source_run_id": d.sourceRunID})
			if _, err := etx.Ledger().Append(ctx, ledger.Record{RecordType: ledger.RecordDecision, RunID: newRunID, Origin: ledger.Origin{Kind: ledger.OriginEngine, ActorID: producerActorID}, Authority: ledger.AuthorityDerived, Data: data}); err != nil {
				return err
			}
			if _, err := tx.q.Exec(ctx, `UPDATE trigger_remints SET status='minted',minted_run_id=$2,updated_at=$3 WHERE id=$1`, d.id, newRunID, now); err != nil {
				return err
			}
			admitted++
		}
		return nil
	})
	return admitted, err
}
