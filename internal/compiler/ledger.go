package compiler

import (
	"fmt"
	"strconv"
	"strings"
)

// checkLedger is the §11.4 ledger level: the projections a node reads and the
// record types it may write exist in the vocabulary, authority is not
// over-claimed, and success signals name checks the system can run.
func (c *compilation) checkLedger() {
	for _, id := range c.nodeIDs {
		n := c.doc.Spec.Nodes[id]
		base := pointerJoin("/spec/nodes", id)
		c.checkLedgerDelta(base, id, n)
		c.checkAcceptance(base, n)
		c.checkProjectionBindings(base, n)
	}
}

func (c *compilation) checkLedgerDelta(base, id string, n *node) {
	if n.Ledger == nil {
		return
	}

	for i, name := range n.Ledger.Read {
		if !ledgerProjections[name] {
			c.add(LevelError, base+"/ledger/read/"+strconv.Itoa(i), CodeLedgerProjectionUnknown,
				fmt.Sprintf("node %q reads projection %q, which is not in the ledger vocabulary", id, name),
				fmt.Sprintf("use one of: %s", strings.Join(sortedKeys(ledgerProjections), ", ")))
		}
	}

	for _, field := range []struct {
		name  string
		types []string
	}{{"propose", n.Ledger.Propose}, {"observe", n.Ledger.Observe}} {
		for i, recordType := range field.types {
			if !ledgerRecordTypes[recordType] {
				c.add(LevelError, base+"/ledger/"+field.name+"/"+strconv.Itoa(i), CodeLedgerRecordTypeUnknown,
					fmt.Sprintf("node %q declares %s of record type %q, which is not in the ledger vocabulary", id, field.name, recordType),
					fmt.Sprintf("use one of: %s", strings.Join(sortedKeys(ledgerRecordTypes), ", ")))
			}
		}
	}

	// PRD §10.4: only a trusted runner issues `observed` evidence, and only
	// for facts it directly measured. An agent node claiming `observe` is
	// claiming an authority the ledger runtime would refuse at append time —
	// better to refuse it here, where the author can see it.
	if len(n.Ledger.Observe) > 0 && n.Kind != KindCode {
		c.add(LevelError, base+"/ledger/observe", CodeLedgerObserveNotPermitted,
			fmt.Sprintf("node %q is of kind %q and cannot issue observed records; only a trusted runner observes facts it measured directly (PRD §10.4)", id, n.Kind),
			"move the declaration to propose, or run the work as a code node through the runner boundary")
	}
}

// checkAcceptance checks mechanical acceptance kinds against the checks the
// PRD names (§10.10). Unknown kinds warn rather than fail: the check registry
// is later work, and an unrecognised check is a gap in this compiler's
// knowledge, not proof the author is wrong.
func (c *compilation) checkAcceptance(base string, n *node) {
	if n.Acceptance == nil {
		return
	}
	for i, requirement := range n.Acceptance.Requires {
		kind, _ := requirement["kind"].(string)
		if kind == "" || acceptanceKinds[kind] {
			continue
		}
		c.add(LevelWarning, base+"/acceptance/requires/"+strconv.Itoa(i)+"/kind", CodeLedgerAcceptanceUnknown,
			fmt.Sprintf("acceptance check %q is not a check this compiler knows how to run", kind),
			fmt.Sprintf("known checks are: %s; an unknown check cannot be mechanically enforced yet", strings.Join(sortedKeys(acceptanceKinds), ", ")))
	}
}

// checkProjectionBindings validates the projection *names* reached through
// input bindings. The contract level already proved these pointers are
// well-formed and start at /ledger/projections.
func (c *compilation) checkProjectionBindings(base string, n *node) {
	if n.Input == nil {
		return
	}
	if n.Input.From != "" {
		c.checkProjectionPointer(base+"/input/from", n.Input.From)
	}
	for _, key := range sortedKeys(n.Input.Bindings) {
		c.checkProjectionPointer(pointerJoin(base+"/input/bindings", key), n.Input.Bindings[key])
	}
}

func (c *compilation) checkProjectionPointer(path, pointer string) {
	tokens, err := parsePointer(pointer)
	if err != nil || len(tokens) < 3 || tokens[0] != bindingRootLedger || tokens[1] != "projections" {
		return
	}
	if ledgerProjections[tokens[2]] {
		return
	}
	c.add(LevelError, path, CodeLedgerProjectionUnknown,
		fmt.Sprintf("binding %q names projection %q, which is not in the ledger vocabulary", pointer, tokens[2]),
		fmt.Sprintf("use one of: %s", strings.Join(sortedKeys(ledgerProjections), ", ")))
}
