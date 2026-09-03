package worker

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

const HostnameEnv = "NODES_HOSTNAME"

var containerIDHostname = regexp.MustCompile(`^[0-9a-f]{12}$`)

// PresenceHostname prefers the explicitly configured host identity over the
// container namespace hostname.
func PresenceHostname() (string, error) {
	if hostname := strings.TrimSpace(os.Getenv(HostnameEnv)); hostname != "" {
		return hostname, nil
	}
	return os.Hostname()
}

// HostnameReason makes a likely container ID visible to mesh readers.
func HostnameReason(hostname string) string {
	if containerIDHostname.MatchString(hostname) {
		return "hostname looks like a 12-character container id; configure NODES_HOSTNAME with the host name"
	}
	return ""
}

func (w *Worker) recordPresence(ctx context.Context) error {
	if err := w.db.UpsertWorkerPresence(ctx, postgres.WorkerPresence{
		WorkerID:  w.opts.WorkerID,
		Hostname:  w.opts.Hostname,
		Revision:  w.opts.Revision,
		ActorKeys: w.opts.ActorKeys,
		LastSeen:  w.opts.Now(),
	}); err != nil {
		return fmt.Errorf("worker: record presence: %w", err)
	}
	return nil
}
