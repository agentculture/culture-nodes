package api

// Jira reactions that carry a stamped origin (issue #328, task t38f). This is
// the Go twin of pr_upkeep_jira.jira_reaction_facts, last_cn1_marker and
// _jira_origin (examples/pr-upkeep/pr_upkeep_jira.py); the two must stay
// output-identical, which TestJiraReplayMatchesPythonSweepSeam pins.
//
// A bridge embeds the cn1 marker the engine minted in the artifact it
// creates (adapters/*/src/*/stamping.py stamp_text) and reports the provider
// id it got back; the engine binds that id to the marker. An emitter that
// reads the marker back out of the artifact passes it on as the event's
// `origin`, and internal/declengine/marker.go verify decides whether the
// reaction continues the firing's lineage. The emitter never decides that.

import (
	"regexp"
	"sort"
	"strings"
)

// cn1Marker is stamping.py's _MARKER, character for character.
var cn1Marker = regexp.MustCompile(`^cn1:[^:]{1,256}:[^:]{1,256}:[0-9a-fA-F]{48}:[0-9a-fA-F]{64}$`)

const (
	// jiraIssueMarkerKind is the marker kind a jira.create or jira.transition
	// firing mints (internal/decl/kinds: both produce jira.issue first).
	jiraIssueMarkerKind    = "jira.issue"
	jiraCommentReaction    = "jira.comment"
	jiraTransitionReaction = "jira.issue.transitioned"
)

// lastCN1Marker is the last syntactically valid marker in an artifact's
// text, or "". A bridge stamps it on its own line, so the text is split on
// ASCII whitespace (as the Python twin does) and each token matched whole.
func lastCN1Marker(s string) string {
	found := ""
	for _, token := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
	}) {
		if cn1Marker.MatchString(token) {
			found = token
		}
	}
	return found
}

func markerKind(marker string) string { return strings.Split(marker, ":")[2] }

// jiraOrigin is the payload origin the engine verifies, or nil (fail
// closed). bridgeAccount is the webhook's configured bot account, never
// read from the artifact: an origin without an author or without the
// bridge account would skip verify's author check, so none is attached.
func jiraOrigin(marker, artifactID, author, bridgeAccount string) map[string]any {
	if marker == "" || artifactID == "" || author == "" || bridgeAccount == "" {
		return nil
	}
	return map[string]any{"marker": marker, "artifact_kind": markerKind(marker), "artifact_id": artifactID, "author": author, "bridge_account": bridgeAccount}
}

// jiraCreatedOrigin adds the stamped origin to jira.issue.created's payload
// when the description carries a jira.issue marker. It reports whether a
// marker was withheld for want of a configured bot account.
func jiraCreatedOrigin(issue map[string]any, payload map[string]any, bot string) bool {
	fields := object(issue["fields"])
	marker := lastCN1Marker(jiraText(fields["description"]))
	if marker != "" && bot == "" {
		return true
	}
	if marker != "" && markerKind(marker) == jiraIssueMarkerKind {
		if origin := jiraOrigin(marker, text(issue["id"]), text(object(fields["creator"])["accountId"]), bot); origin != nil {
			payload["origin"] = origin
		}
	}
	return false
}

// jiraReactionFacts is the neutral per-comment reactions: jira.comment for
// every comment a person wrote, and for a bridge comment that carries a
// marker the reaction to the action that posted it -- jira.comment for a
// comment marker, jira.issue.transitioned for a transition's marker comment.
// The legacy pr-upkeep.jira.* facts still drop the system's own comments.
func jiraReactionFacts(issue map[string]any, key, site, bot string) ([]jiraFact, int) {
	fields := object(issue["fields"])
	comments := append([]any(nil), array(object(fields["comment"])["comments"])...)
	sort.SliceStable(comments, func(i, j int) bool {
		return historyLess(text(object(comments[i])["id"]), text(object(comments[j])["id"]))
	})
	var facts []jiraFact
	withheld := 0
	for _, value := range comments {
		comment := object(value)
		id, author := text(comment["id"]), accountID(comment)
		body := jiraText(comment["body"])
		marker := lastCN1Marker(body)
		if marker != "" && bot == "" {
			withheld++
		}
		var origin map[string]any
		if marker != "" {
			origin = jiraOrigin(marker, id, author, bot)
		}
		own := commentIsSelfEcho(comment, bot)
		if own && origin == nil {
			continue
		}
		watermark := marshal(map[string]any{"comment_id": id})
		if own && markerKind(marker) == jiraIssueMarkerKind {
			created := text(comment["created"])
			if created == "" {
				created = text(comment["updated"])
			}
			from, to := jiraStatusAt(issue, created)
			payload := map[string]any{"source": "jira", "issue": key, "to_status": to, "site": site, "origin": origin}
			if from != "" {
				payload["from_status"] = from
			}
			facts = append(facts, jiraFact{Name: jiraTransitionReaction, Payload: marshal(payload), SourceKey: "jira:" + site + ":" + key + ":transitioned:" + to + ":comment:" + id, Watermark: watermark, Subject: key})
			continue
		}
		payload := map[string]any{"source": "jira", "issue": key, "comment_id": id, "author": author, "body": body, "site": site}
		if origin != nil {
			payload["origin"] = origin
		}
		facts = append(facts, jiraFact{Name: jiraCommentReaction, Payload: marshal(payload), SourceKey: "jira:" + site + ":" + key + ":comment:" + id, Watermark: watermark, Subject: key})
	}
	return facts, withheld
}

// jiraStatusAt is the (from, to) of the newest status change at or before
// at: the move a jira.transition marker comment follows. With none recorded
// yet, the issue's current status, from nowhere.
func jiraStatusAt(issue map[string]any, at string) (string, string) {
	type change struct {
		created, id string
		item        map[string]any
	}
	var changes []change
	for _, value := range array(object(issue["changelog"])["histories"]) {
		h := object(value)
		for _, item := range array(h["items"]) {
			if x := object(item); text(x["field"]) == "status" {
				changes = append(changes, change{text(h["created"]), text(h["id"]), x})
			}
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].created != changes[j].created {
			return changes[i].created < changes[j].created
		}
		return historyLess(changes[i].id, changes[j].id)
	})
	var last map[string]any
	for _, c := range changes {
		if c.created <= at {
			last = c.item
		}
	}
	if last == nil {
		return "", text(object(object(issue["fields"])["status"])["name"])
	}
	return text(last["fromString"]), text(last["toString"])
}
