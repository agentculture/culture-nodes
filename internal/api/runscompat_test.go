package api_test

import (
	"context"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
)

// A dispatched declaration uses its firing ID as the execution run ID. The
// read surfaces must expose the causal identity without losing run details.
func TestDeclarationFiringRunSurfaces(t *testing.T) {
	f := newFixture(t)
	run, _ := createMinimalRun(t, f)
	v := publishActiveDecl(t, f.store, f.nsID, explainDecl("compat-read"))
	eventID := deliverEvent(t, f.store, f.nsID)
	_, err := f.store.Pool().Exec(context.Background(), `INSERT INTO declaration_firings
		(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id)
		VALUES($1,$2,$3,$4,$5,'trigger','condition','action',$1,$1)`, run.ID, f.nsID, eventID, v.DeclarationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	childEvent := deliverEvent(t, f.store, f.nsID)
	childID := store.NewULID()
	_, err = f.store.Pool().Exec(context.Background(), `INSERT INTO declaration_firings
		(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id)
		VALUES($1,$2,$3,$4,$5,'trigger','condition','action',$6,$1)`, childID, f.nsID, childEvent, v.DeclarationID, v.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	var view apipkg.RunViewOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.Firing == nil || view.Run.Firing.LineageID != run.ID || view.Run.Firing.DeclarationID != v.DeclarationID {
		t.Fatalf("run firing = %+v", view.Run.Firing)
	}
	if len(view.Firings) != 2 || view.Firings[0].ID != run.ID || view.Firings[1].ID != childID {
		t.Fatalf("lineage firings = %+v", view.Firings)
	}

	var runs apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &runs)
	requireStatus(t, resp, body, http.StatusOK)
	if len(runs.Items) == 0 || runs.Items[0].Firing == nil {
		t.Fatalf("run list = %+v", runs.Items)
	}

	var nodes apipkg.NodeRunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/node-runs"), nil, &nodes)
	requireStatus(t, resp, body, http.StatusOK)
	if len(nodes.Items) == 0 || nodes.Items[0].Firing == nil {
		t.Fatalf("node runs = %+v", nodes.Items)
	}
	_, err = f.store.Pool().Exec(context.Background(), `INSERT INTO human_tasks
		(id,namespace_id,run_id,kind,status,request) VALUES($1,$2,$3,'approval','pending','{}')`, store.NewULID(), f.nsID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tasks apipkg.HumanTaskListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks"), nil, &tasks)
	requireStatus(t, resp, body, http.StatusOK)
	if len(tasks.Items) == 0 || tasks.Items[0].Firing == nil {
		t.Fatalf("human tasks = %+v", tasks.Items)
	}

	actor := f.insertActor("compat-claim")
	appendAgentClaim(t, f, run.ID, actor, "ready")
	var decisions apipkg.PendingDecisionListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions?run_id="+run.ID), nil, &decisions)
	requireStatus(t, resp, body, http.StatusOK)
	if len(decisions.Items) != 1 || decisions.Items[0].Firing == nil {
		t.Fatalf("pending decisions = %+v", decisions.Items)
	}

	_, err = f.store.Pool().Exec(context.Background(), `UPDATE runs SET subject='SCRUM-COMPAT' WHERE id=$1`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ticket apipkg.TicketOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/SCRUM-COMPAT"), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	if len(ticket.Runs) != 1 || ticket.Runs[0].Firing == nil {
		t.Fatalf("ticket runs = %+v", ticket.Runs)
	}
}
