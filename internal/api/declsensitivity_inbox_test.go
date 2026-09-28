package api_test

// Task t40b (#328): two inbox defects a production test found on
// 2026-09-28. GET /v1alpha1/sensitivity-approvals?status=pending replayed a
// stale inbox through the Cloudflare edge (the origin sent no Cache-Control,
// #305), and POST .../{id}/decision appended a second decision to a task
// that was already approved. The inbox must answer from the database on
// every read, filter by the derived state exactly, and refuse a blind second
// decision with 409 while still accepting a deliberate correction that
// names the decision it supersedes.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/declengine"
)

type inboxItem struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	DecisionID string `json:"decision_id"`
}

type inboxDecision struct {
	ID         string `json:"id"`
	Decision   string `json:"decision"`
	Supersedes string `json:"supersedes"`
}

func TestSensitivityInboxStatusFilterAndSingleDecision(t *testing.T) {
	srv, nsID, token, owner := newDeclarationHumanFixture(t)
	var published declarationVersionResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: announceDeclSource("announce", "{summary}", "summary")}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rr.Code, rr.Body.String())
	}
	st := requireStore(t)
	open := func(name, variable string) string {
		return openApprovalNamed(t, st, nsID, name, published.ID, published.DeclarationID, variable, owner)
	}
	decide := func(id string, body map[string]string) (int, inboxDecision, http.Header) {
		var d inboxDecision
		rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/sensitivity-approvals/"+id+"/decision", token, body, nil)
		if rr.Code == http.StatusCreated {
			if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode decision %s: %v", rr.Body.String(), err)
			}
		}
		return rr.Code, d, rr.Header()
	}
	decisions := func(id string) int {
		ds, err := declengine.ListSensitivityDecisions(context.Background(), st, nsID, id)
		if err != nil {
			t.Fatal(err)
		}
		return len(ds)
	}
	list := func(status string) (map[string]inboxItem, http.Header) {
		var inbox struct {
			Items []inboxItem `json:"items"`
		}
		rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/sensitivity-approvals?owner="+owner+"&status="+status, "", nil, &inbox)
		if rr.Code != http.StatusOK {
			t.Fatalf("list %q: %d %s", status, rr.Code, rr.Body.String())
		}
		out := map[string]inboxItem{}
		for _, it := range inbox.Items {
			if status != "" && it.Status != status {
				t.Errorf("list status=%s returned %s in state %s", status, it.ID, it.Status)
			}
			out[it.ID] = it
		}
		return out, rr.Header()
	}
	sameIDs := func(what string, got map[string]inboxItem, want ...string) {
		t.Helper()
		var g []string
		for id := range got {
			g = append(g, id)
		}
		sort.Strings(g)
		sort.Strings(want)
		if strings.Join(g, ",") != strings.Join(want, ",") {
			t.Fatalf("%s = %v, want exactly %v", what, g, want)
		}
	}

	// The production shape: one task opened first and approved, then more
	// tasks opened across several declarations by later firings.
	first := open("pr-upkeep-route", "work_item")
	if code, _, h := decide(first, map[string]string{"decision": "approved", "note": "first"}); code != http.StatusCreated {
		t.Fatalf("first decision on a pending task: %d", code)
	} else if h.Get("Cache-Control") != "no-store" {
		t.Errorf("decision Cache-Control = %q, want no-store", h.Get("Cache-Control"))
	}
	refusedID := open("digest", "summary")
	if code, _, _ := decide(refusedID, map[string]string{"decision": "refused"}); code != http.StatusCreated {
		t.Fatalf("refusal: %d", code)
	}
	var pending []string
	for _, k := range [][2]string{{"pr-upkeep-route", "repository"}, {"pr-upkeep-route", "number"}, {"announce", "summary"}, {"digest", "title"}, {"triage", "source"}, {"triage", "head_sha"}} {
		pending = append(pending, open(k[0], k[1]))
	}

	got, h := list(declengine.SensitivityPending)
	sameIDs("pending inbox", got, pending...)
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("inbox Cache-Control = %q, want no-store: an edge may replay a stale inbox (#305)", cc)
	}
	approved, _ := list(declengine.SensitivityApproved)
	sameIDs("approved inbox", approved, first)
	refused, _ := list(declengine.SensitivityRefused)
	sameIDs("refused inbox", refused, refusedID)
	all, _ := list("")
	sameIDs("whole inbox", all, append([]string{first, refusedID}, pending...)...)

	// A blind second decision on a decided task: 409, nothing appended --
	// whichever answer it carries.
	for _, id := range []string{first, refusedID} {
		for _, dec := range []string{declengine.SensitivityApproved, declengine.SensitivityRefused} {
			if code, _, _ := decide(id, map[string]string{"decision": dec, "note": "again"}); code != http.StatusConflict {
				t.Fatalf("second %s on %s: status %d, want 409", dec, id, code)
			}
		}
		if n := decisions(id); n != 1 {
			t.Fatalf("task %s has %d decisions after refused repeats, want 1", id, n)
		}
	}
	// A correction against a stale head is also a conflict.
	if code, _, _ := decide(refusedID, map[string]string{"decision": "approved", "supersedes": "01STALEDECISION"}); code != http.StatusConflict {
		t.Fatalf("stale correction: status %d, want 409", code)
	}
	// The deliberate correction names the current head, and appends.
	head := all[refusedID].DecisionID
	code, corr, _ := decide(refusedID, map[string]string{"decision": "approved", "note": "fine after all", "supersedes": head})
	if code != http.StatusCreated || corr.Supersedes != head || corr.Decision != "approved" {
		t.Fatalf("correction: %d %+v, want 201 superseding %s", code, corr, head)
	}
	if n := decisions(refusedID); n != 2 {
		t.Fatalf("corrected task has %d decisions, want 2", n)
	}
	// A pending task has no head to supersede.
	if code, _, _ := decide(pending[0], map[string]string{"decision": "approved", "supersedes": head}); code != http.StatusConflict {
		t.Fatalf("correction of a pending task: status %d, want 409", code)
	}
	if n := decisions(pending[0]); n != 0 {
		t.Fatalf("pending task gained %d decisions from a refused correction", n)
	}

	// Deciding one pending task moves it, and only it, out of the pending list.
	if code, _, _ := decide(pending[0], map[string]string{"decision": "approved"}); code != http.StatusCreated {
		t.Fatalf("first decision on pending task: %d", code)
	}
	got, _ = list(declengine.SensitivityPending)
	sameIDs("pending inbox after one decision", got, pending[1:]...)
	approved, _ = list(declengine.SensitivityApproved)
	sameIDs("approved inbox after decisions", approved, first, refusedID, pending[0])
}
