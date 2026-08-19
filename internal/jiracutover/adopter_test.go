package jiracutover_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/jiracutover"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

var testStore *postgres.Store

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, func(s *postgres.Store) { testStore = s })) }

type fakeFetcher struct {
	head  postgres.JiraHistoryWatermark
	found bool
}

func (f fakeFetcher) Head(context.Context, string, string) (postgres.JiraHistoryWatermark, bool, error) {
	return f.head, f.found, nil
}

func TestAdoptPendingMarksRowsAndSkipsThemOnRetry(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns, err := s.CreateNamespace(ctx, "cutover-"+store.NewULID(), "Cutover test")
	if err != nil {
		t.Fatal(err)
	}
	key := "jira:team.example.com:SCRUM-203"
	if _, err := s.Pool().Exec(ctx, `INSERT INTO jira_history_watermark_cutovers(namespace_id,issue_source_key) VALUES($1,$2)`, ns.ID, key); err != nil {
		t.Fatal(err)
	}
	n, err := jiracutover.AdoptPending(ctx, s, fakeFetcher{head: postgres.JiraHistoryWatermark{ChangelogID: "42", CommentID: "17"}, found: true}, nil)
	if err != nil || n != 1 {
		t.Fatalf("AdoptPending = %d, %v", n, err)
	}
	var adopted bool
	var changelog string
	if err := s.Pool().QueryRow(ctx, `SELECT adopted_at IS NOT NULL, watermark->>'changelog_id' FROM jira_history_watermark_cutovers WHERE namespace_id=$1 AND issue_source_key=$2`, ns.ID, key).Scan(&adopted, &changelog); err != nil {
		t.Fatal(err)
	}
	if !adopted || changelog != "42" {
		t.Fatalf("adopted=%v changelog=%q", adopted, changelog)
	}
	if n, err := jiracutover.AdoptPending(ctx, s, fakeFetcher{}, nil); err != nil || n != 0 {
		t.Fatalf("idempotent retry = %d, %v", n, err)
	}
}

func TestAdoptPendingMissingIssueUsesEmptyHeadAndLogs(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns, err := s.CreateNamespace(ctx, "cutover-missing-"+store.NewULID(), "Cutover missing test")
	if err != nil {
		t.Fatal(err)
	}
	key := "jira:team.example.com:GONE-1"
	if _, err := s.Pool().Exec(ctx, `INSERT INTO jira_history_watermark_cutovers(namespace_id,issue_source_key) VALUES($1,$2)`, ns.ID, key); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if n, err := jiracutover.AdoptPending(ctx, s, fakeFetcher{found: false}, log.New(&logs, "", 0)); err != nil || n != 1 {
		t.Fatalf("AdoptPending = %d, %v", n, err)
	}
	var watermark string
	if err := s.Pool().QueryRow(ctx, `SELECT watermark::text FROM jira_history_watermark_cutovers WHERE namespace_id=$1 AND issue_source_key=$2`, ns.ID, key).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(watermark, `"changelog_id": ""`) || !strings.Contains(logs.String(), "no longer returned") {
		t.Fatalf("watermark=%s logs=%q", watermark, logs.String())
	}
}
