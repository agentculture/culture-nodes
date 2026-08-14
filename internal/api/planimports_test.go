package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// Plan import (task t22, issue #45): POST /v1alpha1/plan-imports over the
// real internal/devague testdata fixtures -- the same plan-show.json /
// deviations.json pair internal/devague's own package-level tests prove
// MapPlanShow/MapDeviations against, exercised here end-to-end through the
// HTTP route and the real Postgres-backed store.

type planImportTaskWire struct {
	TaskRef            string   `json:"task_ref"`
	Summary            string   `json:"summary"`
	OriginKind         string   `json:"origin_kind"`
	SourceStatus       string   `json:"source_status"`
	DependsOn          []string `json:"depends_on"`
	Wave               *int     `json:"wave"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Covers             []string `json:"covers"`
}

type planImportDeviationWire struct {
	DeviationRef   string   `json:"deviation_ref"`
	What           string   `json:"what"`
	TaskRef        string   `json:"task_ref"`
	Reason         string   `json:"reason"`
	Affects        []string `json:"affects"`
	OriginKind     string   `json:"origin_kind"`
	SourceStatus   string   `json:"source_status"`
	Classification string   `json:"classification"`
}

type planImportWire struct {
	ID           string                    `json:"id"`
	Slug         string                    `json:"slug"`
	Title        string                    `json:"title"`
	SourceSlug   string                    `json:"source_slug"`
	SourceStatus string                    `json:"source_status"`
	SourceDigest string                    `json:"source_digest"`
	ImportedAt   string                    `json:"imported_at"`
	Tasks        []planImportTaskWire      `json:"tasks"`
	Deviations   []planImportDeviationWire `json:"deviations"`
}

func readDevagueTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../devague/testdata/" + name)
	if err != nil {
		t.Fatalf("read internal/devague/testdata/%s: %v", name, err)
	}
	return data
}

// planImportRequestWire is the request-body escape hatch doJSON's
// json.Marshal(body) call needs: passing already-encoded bytes straight
// through as the JSON value they already are (json.RawMessage implements
// json.Marshaler as itself), so this test sends the EXACT devague testdata
// bytes, not a re-encoded copy that could silently diverge from what
// MapPlanShow's/MapDeviations' own tests exercise.
type planImportRequestWire struct {
	PlanShow   json.RawMessage `json:"plan_show"`
	Deviations json.RawMessage `json:"deviations,omitempty"`
}

func TestImportPlan_RoundTripsRealDependencyEdgesAndPerTaskStatus(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{
		PlanShow:   readDevagueTestdata(t, "plan-show.json"),
		Deviations: readDevagueTestdata(t, "deviations.json"),
	}

	var created planImportWire
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &created)
	requireStatus(t, resp, body, http.StatusCreated)

	if created.ID == "" {
		t.Fatal("importPlan returned an empty id")
	}
	if created.Slug != "t22fixture" || created.SourceSlug != "t22fixture" {
		t.Fatalf("created = %+v, want slug/source_slug t22fixture", created)
	}
	if len(created.Tasks) != 5 {
		t.Fatalf("got %d tasks, want 5", len(created.Tasks))
	}
	if len(created.Deviations) != 3 {
		t.Fatalf("got %d deviations, want 3", len(created.Deviations))
	}

	byRef := map[string]planImportTaskWire{}
	for _, task := range created.Tasks {
		byRef[task.TaskRef] = task
	}

	// The t22 acceptance test, exercised at the HTTP boundary: t3's real
	// dependency is only t1, even though t2 is also in t1's wave.
	t3 := byRef["t3"]
	if len(t3.DependsOn) != 1 || t3.DependsOn[0] != "t1" {
		t.Fatalf("t3.depends_on = %v, want exactly [t1]", t3.DependsOn)
	}

	// Per-task status is not flattened to one value.
	statuses := map[string]bool{}
	for _, task := range created.Tasks {
		statuses[task.SourceStatus] = true
	}
	if len(statuses) < 3 {
		t.Fatalf("only %d distinct task source_status values, want at least 3 (proposed/confirmed/rejected)", len(statuses))
	}

	// GET /v1alpha1/plan-imports/{id} reads the same snapshot back.
	var fetched planImportWire
	getResp, getBody := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports/"+created.ID), nil, &fetched)
	requireStatus(t, getResp, getBody, http.StatusOK)
	if fetched.ID != created.ID || len(fetched.Tasks) != 5 || len(fetched.Deviations) != 3 {
		t.Fatalf("GET plan import = %+v, want it to match the created snapshot", fetched)
	}
}

// TestImportPlan_DeviationsCarryTheirOrigin is the t22 acceptance test for
// deviations, exercised at the HTTP boundary.
func TestImportPlan_DeviationsCarryTheirOrigin(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{
		PlanShow:   readDevagueTestdata(t, "plan-show.json"),
		Deviations: readDevagueTestdata(t, "deviations.json"),
	}
	var created planImportWire
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &created)
	requireStatus(t, resp, body, http.StatusCreated)

	byRef := map[string]planImportDeviationWire{}
	for _, d := range created.Deviations {
		byRef[d.DeviationRef] = d
	}
	d1, d2 := byRef["d1"], byRef["d2"]
	if d1.OriginKind != "human" {
		t.Errorf("d1.origin_kind = %q, want human (user-reported)", d1.OriginKind)
	}
	if d2.OriginKind != "agent" {
		t.Errorf("d2.origin_kind = %q, want agent (system-knows)", d2.OriginKind)
	}
}

// TestImportPlan_MalformedPlanShowIsRefusedWithAHint is the t22 acceptance
// test for malformed input: refused with 400 and a hint, never a 500, never
// a partial import.
func TestImportPlan_MalformedPlanShowIsRefusedWithAHint(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{
		PlanShow: json.RawMessage(`{"slug": "p", "tasks": [
			{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed", "deps": ["t99"]}
		]}`),
	}
	var apiErr apiErrorBody
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &apiErr)
	requireStatus(t, resp, body, http.StatusBadRequest)

	decoded := decodeAPIError(t, body)
	if decoded.Remediation == "" {
		t.Fatal("malformed plan_show response has no remediation/hint")
	}

	// Nothing was imported: this plan's slug has zero snapshots.
	list, err := f.store.ListPlanImports(t.Context(), f.nsID, "p")
	if err != nil {
		t.Fatalf("ListPlanImports: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListPlanImports after a refused import returned %d rows, want 0 (no partial import)", len(list))
	}
}

// TestImportPlan_MissingPlanShowIsRefused proves the required-field case
// is a domain refusal (400), never a panic on a nil/empty body.
func TestImportPlan_MissingPlanShowIsRefused(t *testing.T) {
	f := newFixture(t)

	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), map[string]any{}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestImportPlan_DeviationsPlanSlugMismatchIsRefused proves a deviations
// document for a DIFFERENT plan than plan_show is refused rather than
// silently attached to the wrong plan.
func TestImportPlan_DeviationsPlanSlugMismatchIsRefused(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{
		PlanShow:   readDevagueTestdata(t, "plan-show.json"),
		Deviations: json.RawMessage(`{"plan_slug": "some-other-plan", "deviations": []}`),
	}
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestImportPlan_GetUnknownIDIs404 rounds out the contract_test.go-style
// sweep with a direct 404 check for this route's GET side.
func TestImportPlan_GetUnknownIDIs404(t *testing.T) {
	f := newFixture(t)

	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports/does-not-exist"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

// TestImportPlan_ReimportIsANewSnapshot proves the HTTP surface exposes
// the store's "every import is its own row" behavior: importing the same
// bytes twice yields two distinct ids, not an overwrite.
func TestImportPlan_ReimportIsANewSnapshot(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{PlanShow: readDevagueTestdata(t, "plan-show.json")}

	var first, second planImportWire
	resp1, body1 := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &first)
	requireStatus(t, resp1, body1, http.StatusCreated)
	resp2, body2 := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &second)
	requireStatus(t, resp2, body2, http.StatusCreated)

	if first.ID == second.ID {
		t.Fatal("two imports of the same plan_show produced the same id, want two independent snapshots")
	}
	if first.SourceDigest != second.SourceDigest {
		t.Fatalf("source_digest differs across two imports of byte-identical content: %q vs %q", first.SourceDigest, second.SourceDigest)
	}
}

// planImportSummaryWire is components.schemas.PlanImportSummary.
type planImportSummaryWire struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	SourceSlug   string `json:"source_slug"`
	SourceStatus string `json:"source_status"`
	SourceDigest string `json:"source_digest"`
	ImportedAt   string `json:"imported_at"`
}

type planImportSummaryListWire struct {
	Items []planImportSummaryWire `json:"items"`
}

// TestListPlanImports_MostRecentFirst is task t23's list-by-slug route
// (GET /v1alpha1/plan-imports?slug=), added because t22 deliberately left
// it out: a dashboard needs "every snapshot of this plan, newest first" to
// find the current one without guessing at a supersedes chain the schema
// does not model.
func TestListPlanImports_MostRecentFirst(t *testing.T) {
	f := newFixture(t)

	req := planImportRequestWire{PlanShow: readDevagueTestdata(t, "plan-show.json")}
	var first, second planImportWire
	resp1, body1 := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &first)
	requireStatus(t, resp1, body1, http.StatusCreated)
	resp2, body2 := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/plan-imports"), req, &second)
	requireStatus(t, resp2, body2, http.StatusCreated)

	var list planImportSummaryListWire
	listResp, listBody := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports?slug=t22fixture"), nil, &list)
	requireStatus(t, listResp, listBody, http.StatusOK)

	if len(list.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(list.Items))
	}
	if list.Items[0].ID != second.ID || list.Items[1].ID != first.ID {
		t.Fatalf("list order = [%s, %s], want the more recent import (%s) first",
			list.Items[0].ID, list.Items[1].ID, second.ID)
	}
	for _, item := range list.Items {
		if item.Slug != "t22fixture" || item.SourceDigest == "" {
			t.Fatalf("summary item missing plan-level fields: %+v", item)
		}
	}
}

// TestListPlanImports_UnknownSlugIsEmpty proves an unimported slug is an
// honest empty list, not a 404 — the same "nothing found yet" reading a
// dashboard's empty state renders for.
func TestListPlanImports_UnknownSlugIsEmpty(t *testing.T) {
	f := newFixture(t)

	var list planImportSummaryListWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports?slug=never-imported"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 0 {
		t.Fatalf("got %d items for an unimported slug, want 0", len(list.Items))
	}
}

// TestListPlanImports_MissingSlugIsRefused proves the required-parameter
// case is a domain refusal (400) naming what is wrong, never a panic and
// never a silent "every plan" listing this schema has no basis for.
func TestListPlanImports_MissingSlugIsRefused(t *testing.T) {
	f := newFixture(t)

	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/plan-imports"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}
