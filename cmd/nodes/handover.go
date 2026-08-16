package main

import (
	"fmt"
	"os"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The deployment surface for handover evidence (task t10, issue #13): when a
// run hands its changes over as a git ref, the control plane fetches that ref
// and records the ref, commit sha and changed paths IT measured as an
// `observed` ledger record.
//
// Off unless configured, and configured means naming two things the control
// plane cannot invent for itself:
//
//   - the REMOTE to fetch from. It is deliberately the operator's own
//     configuration and never the remote a bridge reported, because fetching
//     from an agent-supplied url would let a session choose the repository
//     its own work is measured against (see internal/handover's package doc).
//   - the ACTOR the observation is attributed to. ledger_records
//     .origin_actor_id is a foreign key to actors(id), and PRD §10.4 admits
//     observed evidence only from an identified producer, so this is a row
//     somebody registered — exactly like NODES_CODE_RUNNER_ACTOR_ID.
//
// With neither set, nothing is fetched and nothing is recorded, and every
// path behaves as it did before this existed. That is the honest default: a
// deployment that cannot look must not write a record saying it did.
const (
	// envHandoverRemote is the git remote handover refs are fetched from.
	// Any url this host's git can reach (ssh, https, or a filesystem path
	// for a single-host deployment).
	envHandoverRemote = "NODES_HANDOVER_REMOTE"
	// envHandoverActorID names the REGISTERED actors-table row the
	// observation is attributed to.
	envHandoverActorID = "NODES_HANDOVER_ACTOR_ID"
	// envHandoverRevision optionally pins the measuring producer's revision.
	envHandoverRevision = "NODES_HANDOVER_ACTOR_REVISION"
	// envHandoverObjectDir optionally points at a persistent bare repository
	// fetches accumulate objects in, so repeated fetches from one remote do
	// not re-download shared history. Unset means a fresh temporary one per
	// fetch: correct, just slower.
	envHandoverObjectDir = "NODES_HANDOVER_OBJECT_DIR"
)

// handoverObserver builds the observer from the environment, or returns nil
// when the deployment has not configured one.
//
// Half-configuration is an ERROR rather than a silent disable: an operator who
// set a remote and forgot the actor id has said plainly that they expect
// handed-over refs to be measured, and quietly recording nothing would leave
// them believing evidence exists when none does — the exact failure this task
// was written to end.
func handoverObserver(db *postgres.Store, namespaceID string) (*handover.Observer, *clifmt.CliError) {
	remote := os.Getenv(envHandoverRemote)
	actorID := os.Getenv(envHandoverActorID)
	if remote == "" && actorID == "" {
		return nil, nil
	}
	if remote == "" || actorID == "" {
		return nil, &clifmt.CliError{
			Code: clifmt.ExitEnvError,
			Message: fmt.Sprintf(
				"handover evidence is half-configured: %s and %s must be set together", envHandoverRemote, envHandoverActorID),
			Remediation: fmt.Sprintf(
				"set both (%s is a git remote this host can fetch from; %s is a REGISTERED actors row the observation is attributed to), or unset both to record no handover evidence",
				envHandoverRemote, envHandoverActorID),
		}
	}

	resolved, err := handover.AbsRemote(remote)
	if err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("resolving %s: %v", envHandoverRemote, err),
			Remediation: "give an absolute path, or an ssh/https url",
		}
	}

	ledgerRuntime, err := postgres.NewLedger(db, namespaceID)
	if err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the ledger runtime for handover evidence: %v", err),
			Remediation: "check the namespace and database configuration",
		}
	}

	return &handover.Observer{
		Fetcher: &handover.GitFetcher{
			Remote:    resolved,
			ObjectDir: os.Getenv(envHandoverObjectDir),
		},
		Ledger:        ledgerRuntime,
		ActorID:       actorID,
		ActorRevision: os.Getenv(envHandoverRevision),
		// A fetch that could not happen is a fact about this process, not
		// about the run: it goes to the diagnostic stream an operator reads,
		// never to the ledger.
		OnError: func(err error) { clifmt.EmitDiagnostic(err.Error()) },
	}, nil
}
