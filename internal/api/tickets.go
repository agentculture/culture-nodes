package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
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

type TicketOut struct {
	TicketID    string               `json:"ticket_id"`
	Runs        []RunOut             `json:"runs"`
	Ledger      []TicketRunLedgerOut `json:"ledger"`
	HumanTasks  []HumanTaskOut       `json:"human_tasks"`
	Reports     []TicketReportOut    `json:"ticket_reports"`
	Replies     []TicketReplyOut     `json:"replies"`
	LatestFrame *TicketFrameOut      `json:"latest_frame,omitempty"`
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
	rows, err := s.Store.Pool().Query(r.Context(), `SELECT id,run_id,phase,status,payload,created_at
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
	writeJSON(w, http.StatusOK, out)
	return nil
}
