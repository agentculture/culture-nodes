// Engine-derived node types and start_from matching (task t38d, #328, owner
// decision d6, ADR 0014 addendum "Event mapping and start nodes").
//
// A declaration may start from any open node (`start_from: any`) or from
// open nodes of a type (`{host: thor}`, `{actor_kind: codex}`, both = AND).
// The types are what keeps that truthful, so where they come from is the
// whole design:
//
//   - They are columns on declaration_nodes (migration 0069) that only this
//     package writes: Finish sets actor_kind from the action kind the engine
//     dispatched (human.ask -> human, code.run -> code), and
//     RecordNodeTypes fills what is still unset from the firing run's newest
//     attempt -- the actors row that attempt recorded (attempts.actor_id, set
//     by the worker when it dispatched): kind 'human' -> human; otherwise
//     metadata.harness when it is in the closed decl.ActorKinds set; and
//     capabilities.preflight.host.hostname -> host. A registration is written
//     by an operator (POST /v1alpha1/actors needs the registration secret,
//     deploy/prod/register-actor.sh), and the hostname in it is the fact the
//     bridge measured on its own host.
//   - Nothing an author or agent writes is ever read: not the event payload
//     (EventFromSignal never sets them), not the declaration body (a
//     declaration can only ASK for a type), not the action's `with.uses`
//     (an author's choice of target, which the worker's liveness fallback may
//     not even honour -- the attempt records who actually ran).
//   - A fact the engine did not record leaves the column NULL. There is no
//     inference from an actor key, an endpoint URL or a node name. A NULL
//     type matches no typed start_from; `any` still matches.
//   - A set column is never rewritten (COALESCE), so a node's types, once
//     recorded, are stable for every later reaction and for explain.
//
// start_from only ever applies to the node an event LEGITIMATELY arrived at:
// Handle derives that node from a verified origin marker (deriveNode), and
// an event without one arrives at root (EventFromSignal), which is not an
// engine-opened node and so matches no start_from declaration at all.
package declengine

import (
	"context"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/decl"
)

// OutcomeStartUnmatched is recorded when a start_from declaration's trigger
// (kind and `with` filter) matched an event but the node the event arrived
// at did not have the types it needs -- the one non-match the firing loop
// records, so explain can answer "why didn't it fire" by naming the types.
// Declarations without start_from keep the old behaviour: a start-node
// mismatch is silent.
const OutcomeStartUnmatched = "start not matched"

// nodeArrival is what Handle learned about the node the event arrived at.
// It is unexported, so it is never serialized with a stored (frozen or
// deferred) event and never set by a caller: Handle clears it and only
// deriveNode fills it, from the NodeRecord of a verified parent's landing
// node.
type nodeArrival struct {
	opened          bool
	host, actorKind string
}

// NodeTypeBackend is the optional capability that fills a node's
// engine-derived types (see this file's doc comment for the facts read).
type NodeTypeBackend interface {
	// RecordNodeTypes fills the unset host/actor_kind of the open node
	// firingID opened, from the firing run's newest attempt's actor
	// registration. A node with no such attempt keeps what it has.
	RecordNodeTypes(ctx context.Context, namespaceID, firingID string) error
	// RecordSettledNodeTypes does the same for every open node in the
	// namespace whose run has reached a terminal status (the Driver pass).
	RecordSettledNodeTypes(ctx context.Context, namespaceID string) (int, error)
}

// actionActorKind is the actor kind the dispatched action itself decides:
// a human.ask is answered by a person through the control plane's human-task
// surface, a code.run is executed by a runner. Every other action's kind is
// read from the attempt's actor registration once it exists.
func actionActorKind(actionKind string) string {
	switch actionKind {
	case "human.ask":
		return "human"
	case "code.run":
		return "code"
	}
	return ""
}

// startMatch reports whether d starts from the node event arrived at, and
// for a start_from declaration that does not, why (for OutcomeStartUnmatched).
func startMatch(d decl.Declaration, event Event) (ok bool, miss string) {
	if d.StartFrom == nil {
		return d.StartNode.Name == event.Node, ""
	}
	if !event.arrival.opened {
		return false, fmt.Sprintf("start_from %s needs an open node the engine recorded; the event arrived at %q, which no firing opened (an event without a verified origin marker always arrives at %q)",
			d.StartFrom, event.Node, RootNode)
	}
	if d.StartFrom.Matches(event.arrival.host, event.arrival.actorKind) {
		return true, ""
	}
	return false, fmt.Sprintf("start_from needs %s; node %q was recorded as %s", needs(*d.StartFrom), event.Node, recorded(event.arrival))
}

func needs(s decl.StartFrom) string {
	var parts []string
	if s.Host != "" {
		parts = append(parts, "host="+s.Host)
	}
	if s.ActorKind != "" {
		parts = append(parts, "actor_kind="+s.ActorKind)
	}
	sort.Strings(parts)
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " and "
		}
		out += p
	}
	return out
}

func recorded(a nodeArrival) string {
	show := func(v string) string {
		if v == "" {
			return "unset"
		}
		return v
	}
	return "host=" + show(a.host) + " actor_kind=" + show(a.actorKind)
}

// recordNodeTypes fills the node's types when the backend can, before the
// node is read for matching. A backend without the capability (the
// in-memory test fakes) records none.
func (e *Engine) recordNodeTypes(ctx context.Context, namespaceID, firingID string) error {
	if tb, ok := e.backend.(NodeTypeBackend); ok {
		return tb.RecordNodeTypes(ctx, namespaceID, firingID)
	}
	return nil
}

// RecordSettledNodeTypes is the Driver's pass: it records the types of every
// open node whose run has settled, so a node's types are visible before any
// reaction to it arrives.
func (e *Engine) RecordSettledNodeTypes(ctx context.Context, namespaceID string) (int, error) {
	if tb, ok := e.backend.(NodeTypeBackend); ok {
		return tb.RecordSettledNodeTypes(ctx, namespaceID)
	}
	return 0, nil
}

// nodeTypeFactsSQL fills unset types from the newest attempt of each open
// node's run's "action" node run (workerEnvelope always names it "action")
// that recorded an actor. An empty $2 selects every node of the namespace; $3
// requires the run to have settled. actor_kind: 'human' for a human actor,
// else metadata.harness when it is one of $4 (decl.ActorKinds); host: the
// registration's measured hostname. COALESCE keeps any column already set.
const nodeTypeFactsSQL = `UPDATE declaration_nodes dn
	SET host=COALESCE(dn.host,f.host), actor_kind=COALESCE(dn.actor_kind,f.actor_kind), updated_at=now()
	FROM (SELECT DISTINCT ON (n.id) n.id AS node_id,
		NULLIF(a.capabilities #>> '{preflight,host,hostname}','') AS host,
		CASE WHEN a.kind='human' THEN 'human'
		     WHEN a.metadata->>'harness' = ANY($4::text[]) THEN a.metadata->>'harness' END AS actor_kind
		FROM declaration_nodes n
		JOIN runs r ON r.namespace_id=n.namespace_id AND r.id=n.opening_firing_id
		JOIN node_runs nr ON nr.namespace_id=n.namespace_id AND nr.run_id=n.opening_firing_id AND nr.node_key='action'
		JOIN attempts at ON at.node_run_id=nr.id AND at.actor_id IS NOT NULL
		JOIN actors a ON a.id=at.actor_id
		WHERE n.namespace_id=$1 AND n.state='open' AND (n.host IS NULL OR n.actor_kind IS NULL)
		AND ($2='' OR n.opening_firing_id=$2)
		AND (NOT $3 OR r.status IN ('completed','failed','cancelled'))
		ORDER BY n.id, nr.created_at DESC, nr.id DESC, at.attempt_number DESC) f
	WHERE dn.id=f.node_id AND dn.namespace_id=$1
	AND ((dn.host IS NULL AND f.host IS NOT NULL) OR (dn.actor_kind IS NULL AND f.actor_kind IS NOT NULL))`

func actorKindArray() []string {
	out := make([]string, 0, len(decl.ActorKinds))
	for k := range decl.ActorKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RecordNodeTypes implements NodeTypeBackend for one firing's node.
func (p PostgresBackend) RecordNodeTypes(ctx context.Context, namespaceID, firingID string) error {
	if firingID == "" {
		return nil
	}
	_, err := p.Store.Pool().Exec(ctx, nodeTypeFactsSQL, namespaceID, firingID, false, actorKindArray())
	return err
}

// RecordSettledNodeTypes implements NodeTypeBackend for the Driver pass.
func (p PostgresBackend) RecordSettledNodeTypes(ctx context.Context, namespaceID string) (int, error) {
	tag, err := p.Store.Pool().Exec(ctx, nodeTypeFactsSQL, namespaceID, "", true, actorKindArray())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

var _ NodeTypeBackend = PostgresBackend{}
