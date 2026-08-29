package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
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
	// Duplicate is set on the response to a retried POST: the reply above is
	// the one an earlier request with the same reply_id already made.
	Duplicate bool `json:"duplicate,omitempty"`
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
	// ReplyID is the client's idempotency key for this one reply: a retry
	// that carries the same id resolves to the reply already made.
	ReplyID    string `json:"reply_id"`
	Replier    string `json:"replier"`
	Text       string `json:"text"`
	QuestionID string `json:"question_id"`
}

// maxReplyIDLength bounds the client key that becomes part of a source key.
const maxReplyIDLength = 128

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
		return badRequest("send {reply_id, replier, text, question_id?}", "decode ticket reply: %v", err)
	}
	if req.Replier == "" || req.Text == "" {
		return badRequest("replier and text are required", "empty ticket reply")
	}
	if req.ReplyID == "" || len(req.ReplyID) > maxReplyIDLength {
		return badRequest(fmt.Sprintf("reply_id is required: a client-generated key of at most %d characters, reused verbatim when the same reply is retried", maxReplyIDLength), "missing or oversized reply_id")
	}
	// The fact, the reply row that explains it, and the Jira mirror intent
	// are one store transaction, idempotent on reply_id — never three commit
	// boundaries a failure can land between (PR #244, Qodo finding 1).
	delivery, err := s.Store.DeliverPageReply(r.Context(), storepg.DeliverPageReplyInput{
		NamespaceID: s.NamespaceID, TicketID: r.PathValue("id"), ReplyID: req.ReplyID,
		Replier: req.Replier, Text: req.Text, QuestionID: req.QuestionID,
	})
	if err != nil {
		return internalError(err)
	}
	status := http.StatusCreated
	if delivery.Duplicate {
		status = http.StatusOK
	}
	reply := delivery.Reply
	writeJSON(w, status, TicketReplyOut{ID: reply.ID, Replier: reply.Replier, Text: reply.Text, QuestionID: reply.QuestionID,
		SignalEventID: reply.SignalEventID, CreatedAt: reply.CreatedAt, Duplicate: delivery.Duplicate})
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
