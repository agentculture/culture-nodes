package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

type postTicketFrameRequest struct {
	Frame    json.RawMessage `json:"frame"`
	PostedBy string          `json:"posted_by"`
}

type TicketFrameOut struct {
	TicketID  string          `json:"ticket_id"`
	Version   int             `json:"version"`
	Frame     json.RawMessage `json:"frame"`
	PostedBy  string          `json:"posted_by"`
	CreatedAt time.Time       `json:"created_at"`
}

type TicketRunLedgerOut struct {
	RunID   string          `json:"run_id"`
	Records []ledger.Record `json:"records"`
}

type TicketReportOut struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Phase     string          `json:"phase"`
	Status    string          `json:"status"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type TicketReplyOut struct {
	ID            string    `json:"id"`
	Replier       string    `json:"replier"`
	Text          string    `json:"text"`
	QuestionID    string    `json:"question_id,omitempty"`
	SignalEventID string    `json:"signal_event_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type TicketPageLinkOut struct {
	CommentID string `json:"comment_id,omitempty"`
	Status    string `json:"status"`
}

type TicketOut struct {
	TicketID    string               `json:"ticket_id"`
	Runs        []RunOut             `json:"runs"`
	Ledger      []TicketRunLedgerOut `json:"ledger"`
	HumanTasks  []HumanTaskOut       `json:"human_tasks"`
	Reports     []TicketReportOut    `json:"ticket_reports"`
	Replies     []TicketReplyOut     `json:"replies"`
	LatestFrame *TicketFrameOut      `json:"latest_frame,omitempty"`
	PageLink    *TicketPageLinkOut   `json:"page_link,omitempty"`
	Frozen      bool                 `json:"frozen"`
	MergedPR    json.RawMessage      `json:"merged_pr,omitempty"`
}

type postTicketReplyRequest struct {
	// ID is the client's idempotency key for this reply (Qodo 4 on #244): a
	// retry after a 500 resumes the same row instead of minting a second
	// reply and a second engine fact. Optional; the server mints one when
	// absent, in which case a retry IS a new reply.
	ID         string `json:"id"`
	Replier    string `json:"replier"`
	Text       string `json:"text"`
	QuestionID string `json:"question_id"`
}

var ticketReplyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type freezeTicketRequest struct {
	MergedPR json.RawMessage `json:"merged_pr"`
	FrozenBy string          `json:"frozen_by"`
}

func (s *Server) handlePostTicketReply(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	var req postTicketReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send {replier, text, question_id?}", "decode ticket reply: %v", err)
	}
	if req.Replier == "" || req.Text == "" {
		return badRequest("replier and text are required", "empty ticket reply")
	}
	ticketID := r.PathValue("id")
	payload, _ := json.Marshal(map[string]any{
		"id": ticketID, "origin": map[string]any{"kind": "human", "replier": req.Replier},
		"replier": req.Replier, "originating_question_id": req.QuestionID,
		"answer": map[string]any{"comment_id": "ticket-page", "body": req.Text},
	})
	// Order of record (second-opinion review of the wave-1 merge): the reply row
	// lands FIRST with no event, so a failure after delivery can never leave an
	// engine fact that no page reply explains; the event id is attached to the
	// row, with the display-only Jira mirror, once delivery succeeded.
	replyID := req.ID
	if replyID == "" {
		replyID = store.NewULID()
	} else if !ticketReplyIDPattern.MatchString(replyID) {
		return badRequest("id must match ^[A-Za-z0-9_-]{8,64}$", "reply id %q", replyID)
	}
	// A frozen ticket takes no replies (Qodo 5 on #244): the freeze row is
	// read in the same statement that writes the reply, so a freeze landing
	// concurrently cannot slip a reply past it. Zero rows means one of two
	// things, told apart below: frozen, or this id already exists (a retry).
	tag, err := s.Store.Pool().Exec(r.Context(), `INSERT INTO ticket_replies (id,namespace_id,ticket_id,replier,text,question_id)
		SELECT $1,$2,$3,$4,$5,NULLIF($6,'')
		WHERE NOT EXISTS (SELECT 1 FROM ticket_freezes WHERE namespace_id=$2 AND ticket_id=$3)
		ON CONFLICT (id) DO NOTHING`, replyID, s.NamespaceID, ticketID, req.Replier, req.Text, req.QuestionID)
	if err != nil {
		return internalError(err)
	}
	if tag.RowsAffected() == 0 {
		var existing TicketReplyOut
		var eventID *string
		err := s.Store.Pool().QueryRow(r.Context(), `SELECT id, replier, text, coalesce(question_id,''), signal_event_id, created_at FROM ticket_replies WHERE id=$1 AND namespace_id=$2`, replyID, s.NamespaceID).Scan(&existing.ID, &existing.Replier, &existing.Text, &existing.QuestionID, &eventID, &existing.CreatedAt)
		if err == nil {
			if eventID != nil {
				existing.SignalEventID = *eventID
			}
			writeJSON(w, http.StatusOK, existing)
			return nil
		}
		return conflict("the ticket is frozen; replies are closed (see merged_pr on the ticket projection)", "ticket %s is frozen", ticketID)
	}
	delivery, err := s.Store.DeliverSignalEvent(r.Context(), storepg.DeliverSignalEventInput{
		NamespaceID: s.NamespaceID, Name: "pr-upkeep.jira.comment", Payload: payload,
		Emitter: "ticket-page", Subject: ticketID,
	})
	if err != nil {
		return internalError(fmt.Errorf("reply %s recorded without an engine fact: %w", replyID, err))
	}
	comment := fmt.Sprintf("%s\n\nvia %s", req.Text, req.Replier)
	outboxPayload, _ := json.Marshal(map[string]any{"verb": "post_comment", "issue": ticketID, "comment": comment, "phase": "reply", "signal_event_id": delivery.Event.ID})
	tx, err := s.Store.Pool().Begin(r.Context())
	if err != nil {
		return internalError(err)
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `UPDATE ticket_replies SET signal_event_id=$1 WHERE id=$2`, delivery.Event.ID, replyID); err != nil {
		return internalError(err)
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO jira_ticket_report_outbox (id,namespace_id,phase,target_actor_key,issue_key,payload) VALUES ($1,$2,'reply',$3,$4,$5)`, store.NewULID(), s.NamespaceID, storepg.JiraTicketReporterActorKey, ticketID, outboxPayload); err != nil {
		return internalError(err)
	}
	if err = tx.Commit(r.Context()); err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, TicketReplyOut{ID: replyID, Replier: req.Replier, Text: req.Text, QuestionID: req.QuestionID, SignalEventID: delivery.Event.ID, CreatedAt: delivery.Event.CreatedAt})
	return nil
}

func (s *Server) handleFreezeTicket(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	var req freezeTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send {merged_pr, frozen_by}", "decode freeze: %v", err)
	}
	if req.FrozenBy == "" {
		req.FrozenBy = "human"
	}
	if len(req.MergedPR) == 0 {
		req.MergedPR = json.RawMessage(`{}`)
	}
	if !json.Valid(req.MergedPR) {
		return badRequest("merged_pr must be JSON", "invalid merged_pr")
	}
	_, err := s.Store.Pool().Exec(r.Context(), `INSERT INTO ticket_freezes(namespace_id,ticket_id,merged_pr,frozen_by) VALUES($1,$2,$3,$4) ON CONFLICT(namespace_id,ticket_id) DO UPDATE SET merged_pr=EXCLUDED.merged_pr,frozen_by=EXCLUDED.frozen_by`, s.NamespaceID, r.PathValue("id"), req.MergedPR, req.FrozenBy)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket_id": r.PathValue("id"), "frozen": true, "merged_pr": req.MergedPR})
	return nil
}

func (s *Server) handlePostTicketFrame(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	var req postTicketFrameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send {frame, posted_by}", "decode ticket frame: %v", err)
	}
	if len(req.Frame) == 0 || !json.Valid(req.Frame) || req.PostedBy == "" {
		return badRequest("frame must be valid JSON and posted_by is required", "invalid ticket frame request")
	}
	ticketID := r.PathValue("id")
	tx, err := s.Store.Pool().Begin(r.Context())
	if err != nil {
		return internalError(err)
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "ticket-frame:"+s.NamespaceID+":"+ticketID); err != nil {
		return internalError(err)
	}
	var out TicketFrameOut
	err = tx.QueryRow(r.Context(), `INSERT INTO ticket_frames (namespace_id,ticket_id,version,frame_json,posted_by)
		SELECT $1,$2,COALESCE(MAX(version),0)+1,$3::json,$4 FROM ticket_frames WHERE namespace_id=$1 AND ticket_id=$2
		RETURNING ticket_id,version,frame_json,posted_by,created_at`, s.NamespaceID, ticketID, []byte(req.Frame), req.PostedBy).
		Scan(&out.TicketID, &out.Version, &out.Frame, &out.PostedBy, &out.CreatedAt)
	if err != nil {
		return internalError(fmt.Errorf("api: post ticket frame: %w", err))
	}
	if err := tx.Commit(r.Context()); err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) error {
	ticketID := r.PathValue("id")
	runs, err := s.listRuns(r.Context(), listRunsParams{Subject: ticketID, Limit: 500, Sort: sortCreatedAt})
	if err != nil {
		return internalError(err)
	}
	out := TicketOut{TicketID: ticketID, Runs: runs, Ledger: []TicketRunLedgerOut{}, HumanTasks: []HumanTaskOut{}, Reports: []TicketReportOut{}, Replies: []TicketReplyOut{}}
	runSet := make(map[string]bool, len(runs))
	for _, run := range runs {
		runSet[run.ID] = true
		records, err := s.Ledger.Records(r.Context(), run.ID)
		if err != nil {
			return internalError(err)
		}
		if records == nil {
			records = []ledger.Record{}
		}
		out.Ledger = append(out.Ledger, TicketRunLedgerOut{RunID: run.ID, Records: records})
	}
	tasks, err := s.listHumanTasks(r.Context(), "", 500)
	if err != nil {
		return internalError(err)
	}
	for _, task := range tasks {
		if runSet[task.RunID] {
			out.HumanTasks = append(out.HumanTasks, task)
		}
	}
	rows, err := s.Store.Pool().Query(r.Context(), `SELECT id,COALESCE(run_id,''),phase,status,payload,created_at
		FROM jira_ticket_report_outbox WHERE namespace_id=$1 AND issue_key=$2 ORDER BY created_at,id`, s.NamespaceID, ticketID)
	if err != nil {
		return internalError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var report TicketReportOut
		if err := rows.Scan(&report.ID, &report.RunID, &report.Phase, &report.Status, &report.Payload, &report.CreatedAt); err != nil {
			return internalError(err)
		}
		out.Reports = append(out.Reports, report)
		if report.Phase == "page-link" {
			var payload struct {
				CommentID string `json:"comment_id"`
			}
			_ = json.Unmarshal(report.Payload, &payload)
			out.PageLink = &TicketPageLinkOut{CommentID: payload.CommentID, Status: report.Status}
		}
	}
	if err := rows.Err(); err != nil {
		return internalError(err)
	}
	replyRows, err := s.Store.Pool().Query(r.Context(), `SELECT id,replier,text,COALESCE(question_id,''),COALESCE(signal_event_id,''),created_at
		FROM ticket_replies WHERE namespace_id=$1 AND ticket_id=$2 ORDER BY created_at,id`, s.NamespaceID, ticketID)
	if err != nil {
		return internalError(err)
	}
	defer replyRows.Close()
	for replyRows.Next() {
		var reply TicketReplyOut
		if err := replyRows.Scan(&reply.ID, &reply.Replier, &reply.Text, &reply.QuestionID, &reply.SignalEventID, &reply.CreatedAt); err != nil {
			return internalError(err)
		}
		out.Replies = append(out.Replies, reply)
	}
	if err := replyRows.Err(); err != nil {
		return internalError(err)
	}
	var frame TicketFrameOut
	err = s.Store.Pool().QueryRow(r.Context(), `SELECT ticket_id,version,frame_json,posted_by,created_at FROM ticket_frames
		WHERE namespace_id=$1 AND ticket_id=$2 ORDER BY version DESC LIMIT 1`, s.NamespaceID, ticketID).
		Scan(&frame.TicketID, &frame.Version, &frame.Frame, &frame.PostedBy, &frame.CreatedAt)
	if err == nil {
		out.LatestFrame = &frame
	} else if !isNoRowsErr(err) {
		return internalError(err)
	}
	err = s.Store.Pool().QueryRow(r.Context(), `SELECT merged_pr FROM ticket_freezes WHERE namespace_id=$1 AND ticket_id=$2`, s.NamespaceID, ticketID).Scan(&out.MergedPR)
	if err == nil {
		out.Frozen = true
	} else if !isNoRowsErr(err) {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
