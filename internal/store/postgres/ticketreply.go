package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// PageReplyEventName is the signal a human page reply appends. It is the
// same name the pr-upkeep sweep emits for a Jira comment, so one schema and
// one set of subscriptions cover both origins (task t10).
const PageReplyEventName = "pr-upkeep.jira.comment"

// PageReplyEmitter attributes a page-reply fact to the ticket page.
const PageReplyEmitter = "ticket-page"

// DeliverPageReplyInput is one human reply typed on a ticket page.
type DeliverPageReplyInput struct {
	NamespaceID string
	TicketID    string
	// ReplyID is the client-generated idempotency key: it becomes the fact's
	// SourceKey/Watermark cursor, so a retried POST resolves to the reply it
	// already made instead of appending a second fact.
	ReplyID    string
	Replier    string
	Text       string
	QuestionID string
}

// PageReply is one ticket_replies row.
type PageReply struct {
	ID            string
	Replier       string
	Text          string
	QuestionID    string
	SignalEventID string
	CreatedAt     time.Time
}

// PageReplyDelivery is what one DeliverPageReply did: the reply row, the
// fact it explains, and whether both already existed for this ReplyID.
type PageReplyDelivery struct {
	Reply     PageReply
	Event     SignalEvent
	Duplicate bool
}

// PageReplySourceKey is the idempotency cursor of one page reply. Its shape
// is deliberately not one jiraHistoryIssueSourceKey recognises, so the
// pre-cutover history suppression never sees it.
func PageReplySourceKey(ticketID, replyID string) string {
	return "page-reply:" + ticketID + ":" + replyID
}

func (in DeliverPageReplyInput) validate() error {
	switch {
	case in.NamespaceID == "":
		return errors.New("postgres: DeliverPageReply: namespaceID is required")
	case in.TicketID == "":
		return errors.New("postgres: DeliverPageReply: ticketID is required")
	case in.ReplyID == "":
		return errors.New("postgres: DeliverPageReply: replyID is required")
	case in.Replier == "" || in.Text == "":
		return errors.New("postgres: DeliverPageReply: replier and text are required")
	}
	return nil
}

// DeliverPageReply appends the human-origin comment fact, the ticket_replies
// row that explains it, and the display-only Jira mirror intent in ONE
// transaction, idempotent on the client's ReplyID.
//
// Before this seam existed the API handler committed the reply row, then
// called DeliverSignalEvent (its own transaction), then updated the row and
// inserted the mirror in a third — so a failure after the first commit
// returned 500 while leaving a reply with no fact, a failure after the second
// left a fact with no mirror, and the client's retry minted a second reply
// under a fresh ULID either way (PR #244, Qodo finding 1). Here the fact goes
// through the private tx-aware deliverSignalEventTx seam (FireSchedule is
// the precedent) with SourceKey/Watermark set from ReplyID: a redelivery
// finds the durable cursor, appends nothing, and the reply row it already
// wrote is returned with Duplicate set. Any failure rolls back all three
// writes, so there is never a fact without its reply row and mirror, and
// never a reply row without its fact. Migration 0049 makes the one-fact,
// one-reply-row, one-mirror-row shape a unique key rather than a convention.
//
// The mirror's Jira side stays at-least-once (internal/ticketreport posts,
// then marks the row published); this guarantee ends at the outbox row.
func (s *Store) DeliverPageReply(ctx context.Context, in DeliverPageReplyInput) (PageReplyDelivery, error) {
	if err := in.validate(); err != nil {
		return PageReplyDelivery{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"id": in.TicketID, "origin": map[string]any{"kind": "human", "replier": in.Replier},
		"replier": in.Replier, "originating_question_id": in.QuestionID,
		"answer": map[string]any{"comment_id": "ticket-page", "body": in.Text},
	})
	watermark, _ := json.Marshal(map[string]string{"reply_id": in.ReplyID})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	delivery, err := s.deliverSignalEventTx(ctx, tx, DeliverSignalEventInput{
		NamespaceID: in.NamespaceID, Name: PageReplyEventName, Payload: payload,
		Emitter: PageReplyEmitter, Subject: in.TicketID,
		SourceKey: PageReplySourceKey(in.TicketID, in.ReplyID), Watermark: watermark,
	})
	if err != nil {
		return PageReplyDelivery{}, err
	}
	out := PageReplyDelivery{Event: delivery.Event, Duplicate: delivery.Duplicate}
	if delivery.Duplicate {
		// The fact and its reply row were committed together, so the row
		// must exist; an absent one is a broken invariant, not a fresh reply.
		var questionID pgtype.Text
		var createdAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `SELECT id,replier,text,question_id,created_at FROM ticket_replies WHERE signal_event_id=$1`, delivery.Event.ID).
			Scan(&out.Reply.ID, &out.Reply.Replier, &out.Reply.Text, &questionID, &createdAt); err != nil {
			return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: fact %s has no page reply row: %w", delivery.Event.ID, err)
		}
		out.Reply.QuestionID = textOrEmpty(questionID)
		out.Reply.CreatedAt = tsValue(createdAt)
		out.Reply.SignalEventID = delivery.Event.ID
		if err := tx.Commit(ctx); err != nil {
			return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: commit: %w", err)
		}
		return out, nil
	}

	out.Reply = PageReply{ID: store.NewULID(), Replier: in.Replier, Text: in.Text, QuestionID: in.QuestionID, SignalEventID: delivery.Event.ID}
	var createdAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `INSERT INTO ticket_replies (id,namespace_id,ticket_id,replier,text,question_id,signal_event_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7) RETURNING created_at`,
		out.Reply.ID, in.NamespaceID, in.TicketID, in.Replier, in.Text, in.QuestionID, delivery.Event.ID).Scan(&createdAt); err != nil {
		return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: reply row: %w", err)
	}
	out.Reply.CreatedAt = tsValue(createdAt)

	comment := fmt.Sprintf("%s\n\nvia %s", in.Text, in.Replier)
	mirror, _ := json.Marshal(map[string]any{"verb": "post_comment", "issue": in.TicketID, "comment": comment, "phase": "reply", "signal_event_id": delivery.Event.ID})
	if _, err := tx.Exec(ctx, `INSERT INTO jira_ticket_report_outbox (id,namespace_id,phase,target_actor_key,issue_key,payload)
		VALUES ($1,$2,'reply',$3,$4,$5)`, store.NewULID(), in.NamespaceID, JiraTicketReporterActorKey, in.TicketID, mirror); err != nil {
		return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: mirror row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PageReplyDelivery{}, fmt.Errorf("postgres: DeliverPageReply: commit: %w", err)
	}
	return out, nil
}
