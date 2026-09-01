package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

// TicketFreezeOut is what the freeze did to the ticket's runs (task t17,
// spec c28/h19), present only on a frozen ticket.
//
// The counts are DERIVED at read time from the ticket's own runs — a run
// carrying Reason == TicketFrozenReason, counted by its state — not stored
// as a tally the freeze wrote down. A stored counter and the runs it counts
// can disagree; these cannot, because they are the same rows the projection
// already returns in `runs`.
type TicketFreezeOut struct {
	// Reason is the run-level reason stamped on every affected run, so a
	// reader of the banner and a reader of a single run see the same word.
	Reason string `json:"reason"`
	// TicketStatus is the board status the freeze was decided against, as
	// the caller reported it. Empty means the caller did not say — which
	// is why the runs were parked rather than cancelled.
	TicketStatus string `json:"ticket_status,omitempty"`
	Cancelled    int    `json:"cancelled_runs"`
	Parked       int    `json:"parked_runs"`
	// Banner is the sentence the ticket page shows under the frozen
	// heading. It is composed here rather than in the web client so the
	// count a human reads is asserted by the same API test that asserts the
	// runs it counts (spec h19), instead of living only in a component
	// test that mocks the projection it is supposed to be reporting.
	Banner string `json:"banner"`
}

type TicketOut struct {
	TicketID string `json:"ticket_id"`
	// TicketURL is where this ticket lives on the board, composed from the
	// Jira work-item fact the ticket's own runs carry (task t18, spec
	// c10). It is served rather than assembled in the browser so that the
	// one place that knows the fact's shape is the one place that reads
	// it — and so an operator on the page can always get back to Jira
	// without knowing the site by heart. Empty when nothing this
	// projection can see says where the ticket lives; the page then shows
	// no link rather than a guessed one.
	TicketURL string               `json:"ticket_url,omitempty"`
	Runs      []RunOut             `json:"runs"`
	Ledger    []TicketRunLedgerOut `json:"ledger"`
	// HumanTasks is every human task on the ticket's runs, in whatever
	// status. PendingTasks is the decidable subset, shaped for a decider —
	// see TicketPendingTaskOut. Both are present because they answer
	// different questions: the history of what was asked, and what is
	// still being asked.
	HumanTasks   []HumanTaskOut         `json:"human_tasks"`
	PendingTasks []TicketPendingTaskOut `json:"pending_tasks"`
	// PendingRecords is the ticket's undecided ledger claims, grouped by
	// run and quoted at the version this same response read (task t14,
	// spec c11/h5). It is the same shape GET /v1alpha1/pending-decisions
	// serves, narrowed to this ticket's own runs, so a page that renders
	// one can render the other.
	PendingRecords []PendingDecisionRunOut `json:"pending_records"`
	Reports        []TicketReportOut       `json:"ticket_reports"`
	Replies        []TicketReplyOut        `json:"replies"`
	LatestFrame    *TicketFrameOut         `json:"latest_frame,omitempty"`
	PageLink       *TicketPageLinkOut      `json:"page_link,omitempty"`
	Frozen         bool                    `json:"frozen"`
	MergedPR       json.RawMessage         `json:"merged_pr,omitempty"`
	Freeze         *TicketFreezeOut        `json:"freeze,omitempty"`
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
	// TicketStatus is the board status the caller observed on the ticket,
	// e.g. "Done" or "In Progress" (task t17, spec c28). It decides
	// whether the ticket's runs are cancelled or parked, and it has to be
	// supplied rather than looked up: the Jira bridge advertises
	// post_comment / transition_issue / create_issue and no read verb
	// (spec s13/s18), so this control plane has no way to ask. Absent
	// means the caller did not say, and an unsaid status parks — see
	// freezeEndsRunsAsCancelled.
	TicketStatus string `json:"ticket_status"`
}

func (s *Server) handlePostTicketReply(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	var req postTicketReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send {replier, text, question_id?}", "decode ticket reply: %v", err)
	}
	// origin: resolved from principal
	var warning string
	req.Replier, warning = principalActor(r, "replier", req.Replier)
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
	// The mirror names the person, not the id (task t14, spec c16/h8): a
	// reader on the board sees "via Ada Lovelace", and that name is read
	// back out of the actor the verified principal is BOUND to — never the
	// free-text replier the body sent, which by this point a principal has
	// already overridden anyway. Without a principal the field is all this
	// control plane has, and the mirror says so by carrying it unchanged.
	comment := fmt.Sprintf("%s\n\nvia %s", req.Text, s.replierDisplayName(r, req.Replier))
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
	writeJSONWithWarning(w, http.StatusCreated, TicketReplyOut{ID: replyID, Replier: req.Replier, Text: req.Text, QuestionID: req.QuestionID, SignalEventID: delivery.Event.ID, CreatedAt: delivery.Event.CreatedAt}, warning)
	return nil
}

// replierDisplayName is the name the Jira mirror credits a reply to.
//
// With a verified principal it is the bound actor's own display name —
// `metadata.display_name` when the registration recorded one, else the
// actor key — so the name on the board is one a reader can look up in the
// actor registry. It is deliberately NOT the principal's email: the email
// claim is kept for display in this control plane and is not what an actor
// is resolved by (spec c18), and a Jira comment is a public surface.
//
// Any failure to resolve it falls back to the id already stamped on the
// fact rather than failing the reply: the reply and its engine fact are the
// record, and the mirror is display.
func (s *Server) replierDisplayName(r *http.Request, replier string) string {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Synthetic || p.ActorID == "" {
		return replier
	}
	var actorKey string
	var displayName pgtype.Text
	err := s.Store.Pool().QueryRow(r.Context(),
		`SELECT actor_key, metadata->>'display_name' FROM actors WHERE id=$1 AND namespace_id=$2`,
		p.ActorID, s.NamespaceID).Scan(&actorKey, &displayName)
	if err != nil {
		return replier
	}
	if name := strings.TrimSpace(textOrEmpty(displayName)); name != "" {
		return name
	}
	if actorKey != "" {
		return actorKey
	}
	return replier
}

func (s *Server) handleFreezeTicket(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	var req freezeTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send {merged_pr, frozen_by}", "decode freeze: %v", err)
	}
	// origin: resolved from principal
	var warning string
	req.FrozenBy, warning = principalActor(r, "frozen_by", req.FrozenBy)
	if req.FrozenBy == "" {
		req.FrozenBy = "human"
	}
	if len(req.MergedPR) == 0 {
		req.MergedPR = json.RawMessage(`{}`)
	}
	if !json.Valid(req.MergedPR) {
		return badRequest("merged_pr must be JSON", "invalid merged_pr")
	}
	ticketID := r.PathValue("id")
	_, err := s.Store.Pool().Exec(r.Context(), `INSERT INTO ticket_freezes(namespace_id,ticket_id,merged_pr,frozen_by,ticket_status) VALUES($1,$2,$3,$4,NULLIF($5,'')) ON CONFLICT(namespace_id,ticket_id) DO UPDATE SET merged_pr=EXCLUDED.merged_pr,frozen_by=EXCLUDED.frozen_by,ticket_status=EXCLUDED.ticket_status`, s.NamespaceID, ticketID, req.MergedPR, req.FrozenBy, req.TicketStatus)
	if err != nil {
		return internalError(err)
	}
	// The freeze row lands first and separately (task t17, spec c28): the
	// ticket IS frozen the moment the merge is recorded, and it must refuse
	// replies from that instant even if ending its runs then fails. The run
	// walk is the second half, and its failure is reported rather than
	// swallowed — a freeze that left a run running is the exact defect this
	// task exists to close, so it must not be able to look like a success.
	effect, err := s.freezeTicketRuns(r.Context(), ticketID, req.TicketStatus)
	if err != nil {
		return err
	}
	writeJSONWithWarning(w, http.StatusOK, map[string]any{
		"ticket_id":      ticketID,
		"frozen":         true,
		"merged_pr":      req.MergedPR,
		"ticket_status":  req.TicketStatus,
		"cancelled_runs": effect.Cancelled,
		"parked_runs":    effect.Parked,
	}, warning)
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
	// origin: resolved from principal
	var warning string
	req.PostedBy, warning = principalActor(r, "posted_by", req.PostedBy)
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
	writeJSONWithWarning(w, http.StatusCreated, out, warning)
	return nil
}

// ticketPageSize / ticketMaxPages bound every paged read this projection
// makes. A decision surface may not lose a run — or a claim on one — to a
// page limit, so each read walks its cursor to the end rather than serving
// the first page and calling it the answer (task t18).
const (
	ticketPageSize = 500
	ticketMaxPages = 20
)

// ticketRuns is every run addressed to ticketID, newest first.
//
// SubjectFromInput: the ticket page shows what happened on the ticket,
// including runs that predate the runs.subject column and carry the issue
// key only in their input (task t17) — see listRunsParams. It is a method
// rather than inline in handleGetTicket because POST
// /v1alpha1/tickets/{id}/reviews (task t14) has to answer the same question
// — is this run one of the ticket's? — and answering it a second way is how
// the two surfaces would come to disagree about what the ticket owns.
func (s *Server) ticketRuns(ctx context.Context, ticketID string) ([]RunOut, error) {
	runs := make([]RunOut, 0, ticketPageSize)
	var cursor *nodeRunCursor
	for page := 0; page < ticketMaxPages; page++ {
		pageRuns, next, err := s.listRuns(ctx, listRunsParams{
			Subject: ticketID, SubjectFromInput: true, Cursor: cursor,
			Limit: ticketPageSize, Sort: sortCreatedAt,
		})
		if err != nil {
			return nil, err
		}
		runs = append(runs, pageRuns...)
		if next == "" {
			break
		}
		decoded, err := decodeNodeRunCursor(next)
		if err != nil {
			return nil, fmt.Errorf("api: decode ticket run cursor: %w", err)
		}
		cursor = &decoded
	}
	return runs, nil
}

// ticketPendingRecords is the ticket's undecided ledger claims, grouped by
// run and quoted at a version this same response read (task t14, spec
// c11/h5).
//
// The query is GET /v1alpha1/pending-decisions' own, narrowed to the runs
// this projection already listed rather than re-implemented: "proposed, and
// no review record points at it, and nothing supersedes it" is a join with
// three ways to get it subtly wrong, and a ticket page that answered it
// differently from the decisions queue would be a page that disagrees with
// the queue about what is still open.
//
// The version is re-stated from the shared reader for the same reason the
// tasks are: one response, one version per run. Serving records without it
// is what ticketpending.go:44-53 argues against — a client that fetched the
// version separately would be attesting to a frame it never showed anyone.
func (s *Server) ticketPendingRecords(ctx context.Context, runIDs []string, version func(string) (int64, error)) ([]PendingDecisionRunOut, error) {
	out := []PendingDecisionRunOut{}
	if len(runIDs) == 0 {
		return out, nil
	}
	byRun := map[string]int{}
	var cursor *nodeRunCursor
	for page := 0; page < ticketMaxPages; page++ {
		groups, _, next, err := s.listPendingDecisions(ctx, pendingDecisionParams{
			RunIDs: runIDs, Limit: ticketPageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			if at, seen := byRun[group.RunID]; seen {
				out[at].Records = append(out[at].Records, group.Records...)
				continue
			}
			byRun[group.RunID] = len(out)
			out = append(out, group)
		}
		if next == "" {
			break
		}
		decoded, err := decodeNodeRunCursor(next)
		if err != nil {
			return nil, fmt.Errorf("api: decode ticket pending-record cursor: %w", err)
		}
		cursor = &decoded
	}
	for i := range out {
		v, err := version(out[i].RunID)
		if err != nil {
			return nil, err
		}
		out[i].LedgerVersion = v
	}
	return out, nil
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) error {
	ticketID := r.PathValue("id")
	runs, err := s.ticketRuns(r.Context(), ticketID)
	if err != nil {
		return internalError(err)
	}
	out := TicketOut{TicketID: ticketID, Runs: runs, Ledger: []TicketRunLedgerOut{}, HumanTasks: []HumanTaskOut{}, PendingTasks: []TicketPendingTaskOut{}, PendingRecords: []PendingDecisionRunOut{}, Reports: []TicketReportOut{}, Replies: []TicketReplyOut{}}
	// The board link is composed from the runs, which are the only rows in
	// this projection that carry the Jira work-item fact (task t18).
	out.TicketURL = ticketBackLink(ticketID, runs)
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.ID)
		records, err := s.Ledger.Records(r.Context(), run.ID)
		if err != nil {
			return internalError(err)
		}
		if records == nil {
			records = []ledger.Record{}
		}
		out.Ledger = append(out.Ledger, TicketRunLedgerOut{RunID: run.ID, Records: records})
	}
	// Scoped to this ticket's runs, not "the newest N in the namespace then
	// filtered" (task t18): a decision surface may not lose a task to a
	// limit set by unrelated traffic.
	out.HumanTasks, err = s.listHumanTasksForRuns(r.Context(), runIDs)
	if err != nil {
		return internalError(err)
	}
	// The ledger version each pending task carries is READ HERE, in the
	// same request that renders the task — that is what makes the stale
	// guard on POST /human-tasks/{id}/decision mean anything. A client
	// that fetched the version separately would be attesting to a frame it
	// never showed anyone.
	//
	// Both halves of the decidable surface — the tasks and the undecided
	// claims below — quote the version through ONE memoized reader, so a
	// page cannot show a decider two different versions of the same run and
	// have them submit the wrong one.
	versions := runLedgerVersions(func(runID string) (int64, error) {
		return s.Ledger.LedgerVersion(r.Context(), runID)
	})
	out.PendingTasks, err = pendingTicketTasks(out.HumanTasks, versions)
	if err != nil {
		return internalError(err)
	}
	out.PendingRecords, err = s.ticketPendingRecords(r.Context(), runIDs, versions)
	if err != nil {
		return internalError(err)
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
	if out.TicketURL == "" {
		// Second, never first: a posted frame is authored content and the
		// work-item fact above is measured. It is consulted at all because
		// a ticket whose runs predate the fact still has a page, and a
		// page with no way back to the board is the defect c10 names.
		out.TicketURL = ticketFrameBackLink(out.LatestFrame)
	}
	var frozenStatus pgtype.Text
	err = s.Store.Pool().QueryRow(r.Context(), `SELECT merged_pr, ticket_status FROM ticket_freezes WHERE namespace_id=$1 AND ticket_id=$2`, s.NamespaceID, ticketID).Scan(&out.MergedPR, &frozenStatus)
	if err == nil {
		out.Frozen = true
		out.Freeze = ticketFreezeOut(textOrEmpty(frozenStatus), out.Runs)
	} else if !isNoRowsErr(err) {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
