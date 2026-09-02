package api

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// A suite verdict posted under the merge gate's own credential is an
// agent-origin proposed record (task t11 of login-from-anywhere). It is NOT
// the session that did the work, and a later routing must not send a repair
// at the gate because the gate's own verdict happens to be the newest
// agent-origin row.
func TestLastAgentActorIDSkipsTheGatesOwnProposedVerdict(t *testing.T) {
	worker := "01WORKERACTOR00000000000000"
	gate := "01MERGEGATEACTOR00000000000"
	records := []ledger.Record{
		{ID: "r1", RecordType: ledger.RecordClaim, Origin: ledger.Origin{Kind: ledger.OriginAgent, ActorID: worker}, Authority: ledger.AuthorityProposed},
		{ID: "r2", RecordType: ledger.RecordReview, Origin: ledger.Origin{Kind: ledger.OriginAgent, ActorID: gate}, Authority: ledger.AuthorityProposed},
	}
	if got := lastAgentActorID(records); got != worker {
		t.Fatalf("lastAgentActorID = %q, want the worker %q, not the gate's own verdict actor %q", got, worker, gate)
	}
}
