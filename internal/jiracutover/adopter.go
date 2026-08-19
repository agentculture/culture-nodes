package jiracutover

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type HeadFetcher interface {
	Head(context.Context, string, string) (postgres.JiraHistoryWatermark, bool, error)
}

func AdoptPending(ctx context.Context, store *postgres.Store, fetcher HeadFetcher, logger *log.Logger) (int, error) {
	pending, err := store.ListPendingJiraHistoryCutovers(ctx)
	if err != nil {
		return 0, err
	}
	for i, row := range pending {
		parts := strings.Split(row.IssueSourceKey, ":")
		if len(parts) != 3 {
			return i, fmt.Errorf("jiracutover: invalid issue source key %q", row.IssueSourceKey)
		}
		head, found, err := fetcher.Head(ctx, parts[1], parts[2])
		if err != nil {
			return i, fmt.Errorf("jiracutover: fetch %s: %w", row.IssueSourceKey, err)
		}
		if !found && logger != nil {
			logger.Printf("Jira issue %s is no longer returned; adopting empty history head", row.IssueSourceKey)
		}
		raw, _ := json.Marshal(head)
		if err := store.AdoptJiraHistoryHead(ctx, row.NamespaceID, row.IssueSourceKey, raw); err != nil {
			return i, err
		}
	}
	return len(pending), nil
}
