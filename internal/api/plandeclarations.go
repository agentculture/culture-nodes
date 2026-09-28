package api

// Plan import's declaration output (task t35, #328; spec c86, honesty h59):
// the pure, deterministic mapping from a validated devague plan
// (devague.ParsePlanShow) to a declaration set. planimports.go publishes the
// result through publishDeclarationSet when the caller selects
// output=declarations; the default output (the immutable plan snapshot)
// is unchanged.
//
// The mapping, one declaration per ACTIVE task (a rejected task never
// ships, exactly as it occupies no wave in the snapshot):
//
//   - name: plan-<slug>-<task id>, lowercased and reduced to the
//     declaration name alphabet;
//   - a task with no active dependency is a root: it starts on the plan's
//     kickoff node plan-<slug>-start and fires on a human.decision there --
//     a person starts the plan, nothing starts it by itself;
//   - a task with dependencies starts on the landing node of its
//     latest-wave dependency and fires on github.pr.approved (the agent's
//     work lands as a PR; its approval is the task's "done"), and carries a
//     'must' link to EVERY real dependency -- the ADR 0014 join form ("a
//     declaration that must appear after several");
//   - the action is agent.work dispatched to the caller-named actor, whose
//     input carries the task's own summary, instruction and acceptance
//     criteria. That prose is data, not a template: every "{" is escaped as
//     "{{" so it renders literally and never becomes a variable reference;
//   - every node's deadline is the explicit "none": the plan carries no
//     deadlines, and inventing one would be a guess.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/devague"
)

// Plan import output forms. The snapshot stays the default until t37.
const (
	planImportOutputSnapshot     = "snapshot"
	planImportOutputDeclarations = "declarations"
)

func parsePlanImportOutput(raw string) (string, bool) {
	switch raw {
	case "", planImportOutputSnapshot:
		return planImportOutputSnapshot, true
	case planImportOutputDeclarations:
		return planImportOutputDeclarations, true
	}
	return "", false
}

var declarationNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// declarationNamePart lowercases s into the declaration name alphabet,
// bounded so a composed name stays under the schema's 128 characters.
func declarationNamePart(s string, limit int) string {
	out := strings.Trim(declarationNameUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(out) > limit {
		out = strings.Trim(out[:limit], "-")
	}
	if out == "" {
		out = "x"
	}
	return out
}

// escapeTemplate makes s render literally through internal/decl/template.
func escapeTemplate(s string) string { return strings.ReplaceAll(s, "{", "{{") }

// planDeclarationSet maps the plan's active tasks to declarations and their
// dependency edges to must links.
func planDeclarationSet(plan devague.PlanShow, actorRef string) ([]declarationSetItem, []declarationSetLink) {
	prefix := "plan-" + declarationNamePart(plan.Slug, 50)
	active := map[string]devague.PlanTask{}
	for _, t := range plan.Tasks {
		if t.SourceStatus != "rejected" {
			active[t.ID] = t
		}
	}
	nameOf := func(id string) string { return prefix + "-" + declarationNamePart(id, 60) }
	landingOf := func(id string) string { return nameOf(id) + "-done" }
	wave := func(id string) int {
		if w := active[id].Wave; w != nil {
			return *w
		}
		return 0
	}

	var items []declarationSetItem
	var links []declarationSetLink
	for _, t := range plan.Tasks {
		if _, ok := active[t.ID]; !ok {
			continue
		}
		var deps []string
		for _, d := range t.DependsOn {
			if _, ok := active[d]; ok {
				deps = append(deps, d)
			}
		}
		sort.Strings(deps)
		trigger, start := "human.decision", prefix+"-start"
		if len(deps) > 0 {
			latest := deps[0]
			for _, d := range deps[1:] {
				if wave(d) > wave(latest) {
					latest = d
				}
			}
			trigger, start = "github.pr.approved", landingOf(latest)
		}
		criteria := make([]string, 0, len(t.AcceptanceCriteria))
		for _, c := range t.AcceptanceCriteria {
			criteria = append(criteria, escapeTemplate(c))
		}
		body := map[string]any{
			"name":    nameOf(t.ID),
			"trigger": map[string]any{"kind": trigger},
			"action": map[string]any{"kind": "agent.work", "with": map[string]any{
				"uses": actorRef,
				"input": map[string]any{
					"plan":                escapeTemplate(plan.Slug),
					"task_ref":            escapeTemplate(t.ID),
					"summary":             escapeTemplate(t.Summary),
					"instruction":         escapeTemplate(t.Instruction),
					"acceptance_criteria": criteria,
				},
			}},
			"start_node":   map[string]any{"name": start, "deadline": "none"},
			"landing_node": map[string]any{"name": landingOf(t.ID), "deadline": "none"},
		}
		// Marshalling plain maps/strings cannot fail.
		source, _ := json.Marshal(body)
		items = append(items, declarationSetItem{Format: "json", Source: string(source)})
		for _, d := range deps {
			links = append(links, declarationSetLink{From: nameOf(t.ID), To: nameOf(d), Kind: "must"})
		}
	}
	return items, links
}
