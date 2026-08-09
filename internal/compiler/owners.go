package compiler

import "fmt"

// resolveOwner applies the authoring default: a node without its own ownerRef
// inherits the workflow's. Returning the empty string means no owner could be
// resolved at all, which is the one case the owners level reports.
//
// PRD §9.4: authoring defaults may reduce repetition, but the normalized JSON
// must contain a resolved owner for every node.
func resolveOwner(nodeOwner, workflowOwner string) string {
	if nodeOwner != "" {
		return nodeOwner
	}
	return workflowOwner
}

// checkOwners is the §11.4 owners level. Ownership is architecture (PRD §7):
// an unowned node has no one to escalate to, so it is a publication failure
// rather than a warning.
func (c *compilation) checkOwners() {
	for _, id := range c.nodeIDs {
		n := c.doc.Spec.Nodes[id]
		if resolveOwner(n.OwnerRef, c.doc.Metadata.OwnerRef) != "" {
			continue
		}
		c.add(LevelError, pointerJoin("/spec/nodes", id)+"/ownerRef", CodeOwnersUnresolved,
			fmt.Sprintf("node %q has no resolvable owner", id),
			"set the node's ownerRef, or set metadata.ownerRef so every node inherits one (PRD §9.4)")
	}
}
