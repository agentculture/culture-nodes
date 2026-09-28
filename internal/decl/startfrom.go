package decl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// StartFromAny is start_from's one scalar spelling: the declaration may fire
// from any open node an event legitimately arrived at.
const StartFromAny = "any"

// ActorKinds is the closed vocabulary of node actor kinds (task t38d, #328,
// owner decision d6). The engine derives a node's kind from facts it recorded
// itself -- the action kind it dispatched, and the registration of the actor
// its attempt ran on -- never from anything an author or agent writes, so a
// declaration can only ask for one of these, not label a node with it.
var ActorKinds = map[string]bool{
	"claude": true, "codex": true, "qwen": true, "pi": true, "colleague": true,
	"human": true, "code": true,
}

// StartFrom widens a declaration's start from its single declared start node
// to open nodes by type (task t38d, #328, owner decision d6):
//
//   - `start_from: any` matches any open node the event arrived at;
//   - `start_from: {host: thor}`, `{actor_kind: claude}`, or both (AND),
//     match open nodes whose engine-recorded types equal every given key.
//
// A node's types are the engine's record of the action that opened it
// (declaration_nodes.host / actor_kind, migration 0069); a node with a type
// unset matches no typed start_from. Only an engine-opened node has types:
// an event arriving at the shared root node (an outside fact, or any event
// without a verified origin marker) never matches a start_from declaration.
//
// A nil *StartFrom means the field is absent: the declaration starts on its
// start_node exactly as before, and its canonical JSON -- and digest -- are
// unchanged.
type StartFrom struct {
	Any       bool
	Host      string
	ActorKind string
}

type startFromTypes struct {
	Host      string `json:"host,omitempty"`
	ActorKind string `json:"actor_kind,omitempty"`
}

// MarshalJSON renders "any" or the types object, so the canonical JSON and
// the digest change whenever start_from does.
func (s StartFrom) MarshalJSON() ([]byte, error) {
	if s.Any {
		return json.Marshal(StartFromAny)
	}
	return json.Marshal(startFromTypes{Host: s.Host, ActorKind: s.ActorKind})
}

// UnmarshalJSON accepts "any" or an object with only host and/or
// actor_kind. Unknown keys are refused here as well as by the schema, so a
// declaration built in code cannot smuggle one either.
func (s *StartFrom) UnmarshalJSON(raw []byte) error {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		if scalar != StartFromAny {
			return fmt.Errorf("start_from %q: the only scalar value is %q", scalar, StartFromAny)
		}
		*s = StartFrom{Any: true}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var t startFromTypes
	if err := dec.Decode(&t); err != nil {
		return fmt.Errorf("start_from: %w (allowed keys: host, actor_kind)", err)
	}
	*s = StartFrom{Host: t.Host, ActorKind: t.ActorKind}
	return nil
}

// Validate is the check Parse runs after the schema: "any", or at least one
// of host (free-form, non-empty) and actor_kind (from ActorKinds).
func (s StartFrom) Validate() error {
	if s.Any {
		if s.Host != "" || s.ActorKind != "" {
			return errors.New("start_from: \"any\" takes no types")
		}
		return nil
	}
	if s.Host == "" && s.ActorKind == "" {
		return errors.New("start_from: name \"any\", or at least one of host and actor_kind")
	}
	if s.Host != "" && strings.TrimSpace(s.Host) != s.Host {
		return fmt.Errorf("start_from.host %q has surrounding whitespace", s.Host)
	}
	if s.ActorKind != "" && !ActorKinds[s.ActorKind] {
		return fmt.Errorf("start_from.actor_kind %q is not one of %s", s.ActorKind, strings.Join(actorKindList(), ", "))
	}
	return nil
}

// Matches reports whether a node with the given engine-recorded types
// satisfies s. An unset type never matches a key that asks for it.
func (s StartFrom) Matches(host, actorKind string) bool {
	if s.Any {
		return true
	}
	if s.Host != "" && s.Host != host {
		return false
	}
	if s.ActorKind != "" && s.ActorKind != actorKind {
		return false
	}
	return true
}

// String renders s for explain reasons and CLI output: "any", or
// "host=thor actor_kind=codex".
func (s StartFrom) String() string {
	if s.Any {
		return StartFromAny
	}
	var parts []string
	if s.Host != "" {
		parts = append(parts, "host="+s.Host)
	}
	if s.ActorKind != "" {
		parts = append(parts, "actor_kind="+s.ActorKind)
	}
	return strings.Join(parts, " ")
}

func actorKindList() []string {
	out := make([]string, 0, len(ActorKinds))
	for k := range ActorKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
