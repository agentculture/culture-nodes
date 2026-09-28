package main

// `nodes plan-import` (task t22, issue #45): reads `devague plan show
// --json`'s output (and, optionally, a `.devague/deliveries/<slug>.json`
// delivery file) from disk and calls POST /v1alpha1/plan-imports — the
// generic decompose-pipeline import surface (devague is instance one; a
// non-code document, task t24, is instance two). The CLI is a thin client
// of that endpoint, mirroring `nodes run`'s adhocRunRequest pattern: it
// holds no parsing or validation logic of its own (that lives in
// internal/devague, reached only server-side), so the API and CLI lanes
// cannot drift on what counts as a malformed plan.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// planImportRequest mirrors components.schemas.PlanImportRequest
// (api/openapi/openapi.yaml). Both fields carry the source bytes verbatim
// (json.RawMessage), never re-derived: this command's whole job is to hand
// the control plane exactly what `devague` printed/wrote, unmodified.
type planImportRequest struct {
	PlanShow   json.RawMessage `json:"plan_show"`
	Deviations json.RawMessage `json:"deviations,omitempty"`
	// Output and ActorRef select the declaration form (task t35, spec c86);
	// both are omitted for the default snapshot import.
	Output   string `json:"output,omitempty"`
	ActorRef string `json:"actor_ref,omitempty"`
}

// planImportDeclarationSetResponse is the slice of
// components.schemas.DeclarationSet this verb reads.
type planImportDeclarationSetResponse struct {
	Declarations []struct {
		Name string `json:"name"`
	} `json:"declarations"`
	Links     []json.RawMessage `json:"links"`
	Published bool              `json:"published"`
	Warnings  []string          `json:"warnings"`
}

// accessCookieEnv names the variable carrying a human's Cloudflare Access
// session (the same one the Python CLI's declaration verbs and the
// nodes-operator skill read). Environment only, never a flag: a credential
// on argv is visible in ps and shell history.
const accessCookieEnv = "NODES_OP_COOKIE"

// planImportTaskResponse is the slice of components.schemas.PlanImportTask
// this verb reads for its own summary output.
type planImportTaskResponse struct {
	TaskRef      string `json:"task_ref"`
	SourceStatus string `json:"source_status"`
}

// planImportDeviationResponse is the slice of
// components.schemas.PlanImportDeviation this verb reads.
type planImportDeviationResponse struct {
	DeviationRef string `json:"deviation_ref"`
	OriginKind   string `json:"origin_kind"`
}

// planImportResponse is the slice of components.schemas.PlanImport this
// verb reads.
type planImportResponse struct {
	ID           string                            `json:"id"`
	Slug         string                            `json:"slug"`
	Title        string                            `json:"title"`
	SourceSlug   string                            `json:"source_slug"`
	SourceStatus string                            `json:"source_status"`
	ImportedAt   time.Time                         `json:"imported_at"`
	Tasks        []planImportTaskResponse          `json:"tasks"`
	Deviations   []planImportDeviationResponse     `json:"deviations"`
	Declarations *planImportDeclarationSetResponse `json:"declarations"`
}

// planImportResultPayload is `nodes plan-import --json`'s stable result
// shape.
type planImportResultPayload struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	SourceSlug     string `json:"source_slug"`
	SourceStatus   string `json:"source_status"`
	TaskCount      int    `json:"task_count"`
	DeviationCount int    `json:"deviation_count"`
	// Output is "snapshot" or "declarations"; Declarations is present only
	// for the latter. Nothing the verb publishes is active.
	Output       string                 `json:"output"`
	Declarations *planImportDeclSummary `json:"declarations,omitempty"`
}

// planImportDeclSummary is the declaration half of the --json payload.
type planImportDeclSummary struct {
	Count     int      `json:"count"`
	Links     int      `json:"links"`
	Published bool     `json:"published"`
	Warnings  []string `json:"warnings"`
}

// cmdPlanImport implements `nodes plan-import`.
//
// Stream and exit contract, matching `nodes run`'s: the created import's
// id/slug/counts are a result on stdout; exit 0 on success. A malformed
// plan (a missing slug, a task with no id, a dependency edge naming an
// unknown task, a dependency cycle, an unrecognised origin/status) is a
// domain refusal the control plane reports as 400 with a remediation —
// rendered here as a user-error CliError (exit 1), never a panic and never
// a partial import (there is nothing partial to report: the control plane
// either persists the whole snapshot or none of it). An unreachable control
// plane is an environment error (exit 2).
func cmdPlanImport(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("plan-import")
	apiFlag := fs.String("api", "", "control-plane base URL (defaults to NODES_API_URL, then "+defaultAPIBaseURL+")")
	planFlag := fs.String("plan", "", "path to a file holding 'devague plan show --json' output (required)")
	deviationsFlag := fs.String("deviations", "", "path to a .devague/deliveries/<slug>.json delivery file (optional)")
	outputFlag := fs.String("output", "snapshot", "snapshot (default) or declarations: also publish one inactive declaration per active task")
	actorRefFlag := fs.String("actor-ref", "", "the registered actor each task's agent.work action dispatches to (required with --output declarations)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			clifmt.EmitResult(explainPlanImport)
			return clifmt.ExitSuccess, nil
		}
		return 0, parseError("plan-import", err)
	}
	if rest := fs.Args(); len(rest) != 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("plan-import takes no positional arguments, got %q", rest),
			Remediation: "pass the plan file as --plan <path> — run 'nodes plan-import --help' for the full flag list",
		}
	}
	if *planFlag == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "missing required flag: --plan",
			Remediation: "pass --plan <path-to-devague-plan-show-json> — run 'nodes plan-import --help' for the full flag list",
		}
	}

	switch *outputFlag {
	case "snapshot", "declarations":
	default:
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("unknown --output %q", *outputFlag),
			Remediation: "pass --output snapshot (the default) or --output declarations",
		}
	}
	if *outputFlag == "declarations" && *actorRefFlag == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "--output declarations needs --actor-ref",
			Remediation: "pass --actor-ref <actor://...>: the registered actor each task's agent.work action dispatches to",
		}
	}

	planBytes, err := os.ReadFile(*planFlag) // #nosec G304 -- the path is the operator's argument; reading it is the command.
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("cannot read plan file %q: %v", *planFlag, err),
			Remediation: "check the path and that the file is readable; it should hold 'devague plan show --json' output",
		}
	}

	req := planImportRequest{PlanShow: json.RawMessage(planBytes)}
	if *outputFlag == "declarations" {
		req.Output, req.ActorRef = *outputFlag, *actorRefFlag
	}
	if *deviationsFlag != "" {
		deviationsBytes, err := os.ReadFile(*deviationsFlag) // #nosec G304 -- same as above.
		if err != nil {
			return 0, &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("cannot read deviations file %q: %v", *deviationsFlag, err),
				Remediation: "check the path and that the file is readable; it should hold the .devague/deliveries/<slug>.json shape",
			}
		}
		req.Deviations = json.RawMessage(deviationsBytes)
	}

	baseURL := *apiFlag
	if baseURL == "" {
		baseURL = os.Getenv("NODES_API_URL")
	}
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	client := &http.Client{Timeout: runRequestTimeout}
	created, cliErr := postPlanImport(client, baseURL, req)
	if cliErr != nil {
		return 0, cliErr
	}

	if err := emitPlanImportResult(jsonMode, created); err != nil {
		return 0, err
	}
	return clifmt.ExitSuccess, nil
}

// postPlanImport POSTs the request and decodes either the created import or
// the documented {code, message, remediation} error shape — run.go's
// apiErrorToCliError already renders that onto a CliError, reused here so a
// malformed-plan refusal reads identically regardless of which verb hit it.
func postPlanImport(client *http.Client, baseURL string, req planImportRequest) (planImportResponse, *clifmt.CliError) {
	body, err := json.Marshal(req)
	if err != nil {
		return planImportResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("encode request: %v", err),
			Remediation: fmt.Sprintf("this is a CLI fault; file a bug at %s", clifmt.IssuesURL),
		}
	}
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1alpha1/plan-imports", bytes.NewReader(body))
	if err != nil {
		return planImportResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("build request: %v", err),
			Remediation: fmt.Sprintf("this is a CLI fault; file a bug at %s", clifmt.IssuesURL),
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cookie := os.Getenv(accessCookieEnv); cookie != "" {
		httpReq.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: cookie})
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return planImportResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("cannot reach the control plane at %s: %v", baseURL, err),
			Remediation: "check that 'nodes serve' is running there, or point --api / NODES_API_URL at it",
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return planImportResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("read response from %s: %v", baseURL, err),
			Remediation: "check the connection to the control plane and try again",
		}
	}
	if resp.StatusCode != http.StatusCreated {
		return planImportResponse{}, apiErrorToCliError(resp.StatusCode, data)
	}
	var created planImportResponse
	if err := json.Unmarshal(data, &created); err != nil {
		return planImportResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("decode created plan import: %v", err),
			Remediation: "the control plane answered 201 with an unexpected body; check that its version matches this CLI",
		}
	}
	return created, nil
}

// emitPlanImportResult prints the verb's result: the import id, plan
// slug/title, and task/deviation counts.
func emitPlanImportResult(jsonMode bool, created planImportResponse) error {
	payload := planImportResultPayload{
		ID:             created.ID,
		Slug:           created.Slug,
		Title:          created.Title,
		SourceSlug:     created.SourceSlug,
		SourceStatus:   created.SourceStatus,
		TaskCount:      len(created.Tasks),
		DeviationCount: len(created.Deviations),
		Output:         "snapshot",
	}
	if set := created.Declarations; set != nil {
		payload.Output = "declarations"
		payload.Declarations = &planImportDeclSummary{
			Count: len(set.Declarations), Links: len(set.Links), Published: set.Published,
			Warnings: append([]string{}, set.Warnings...),
		}
	}
	if jsonMode {
		return clifmt.EmitResultJSON(payload)
	}
	text := fmt.Sprintf(
		"plan import: %s\nslug: %s\ntitle: %s\ntasks: %d\ndeviations: %d",
		created.ID, created.Slug, created.Title, len(created.Tasks), len(created.Deviations))
	if set := created.Declarations; set != nil {
		text += fmt.Sprintf("\ndeclarations: %d published (inactive: a human activates)\nlinks: %d", len(set.Declarations), len(set.Links))
		for _, d := range set.Declarations {
			text += "\n  " + d.Name
		}
		for _, w := range set.Warnings {
			text += "\nwarning: " + w
		}
	}
	clifmt.EmitResult(text)
	return nil
}

// explainPlanImport is the `nodes explain plan-import` entry and the
// `nodes plan-import --help` text.
const explainPlanImport = `# nodes plan-import

Imports an external plan's faithful view into the control plane (task t22,
issue #45): calls POST /v1alpha1/plan-imports with the exact bytes
` + "`devague plan show --json`" + ` printed (` + "`--plan`" + `) and, optionally, the
exact bytes ` + "`.devague/deliveries/<slug>.json`" + ` holds on disk
(` + "`--deviations`" + `). This is a generic decompose-pipeline surface, not a
code-specific one: devague is instance one, a non-code document is
instance two (task t24).

The imported snapshot carries each task's REAL per-task status and REAL
dependency edges — never a wave-level approximation — and every deviation's
origin (the issue's "system knows" llm vs "user reports" user split).
Nothing is imported partially: a malformed plan or deviations document is
refused with a remediation naming what is wrong.

## Usage

    nodes plan-import --plan plan-show.json
    nodes plan-import --plan plan-show.json --deviations deviations.json
    nodes plan-import --plan plan-show.json --json
    NODES_OP_COOKIE=... nodes plan-import --plan plan-show.json \
        --output declarations --actor-ref actor://company/developer

## Declaration output (task t35)

` + "`--output declarations`" + ` imports the snapshot AND publishes one
declaration per active task: agent.work to ` + "`--actor-ref`" + `, roots firing on a
human.decision at the plan's kickoff node, dependents on github.pr.approved,
with a ` + "`must`" + ` link for every real dependency edge. It needs an
authenticated human principal: the CLI sends ` + "`$NODES_OP_COOKIE`" + ` (a Cloudflare
Access session) as the CF_Authorization cookie, and that principal -- never
a flag -- is the recorded author. Nothing is activated: a human activates
each declaration. Publish warnings (unresolved references, sensitivity
widenings) never refuse and are printed as ` + "`warning:`" + ` lines.

## Flags

- ` + "`--plan`" + ` (required) — path to a file holding
  ` + "`devague plan show --json`" + ` output.
- ` + "`--deviations`" + ` — path to a ` + "`.devague/deliveries/<slug>.json`" + ` file.
- ` + "`--output`" + ` — ` + "`snapshot`" + ` (default) or ` + "`declarations`" + `.
- ` + "`--actor-ref`" + ` — the actor each task dispatches to (required with
  ` + "`--output declarations`" + `).
- ` + "`--api`" + ` — control-plane base URL (default: NODES_API_URL, then
  ` + defaultAPIBaseURL + `).

## Output

Text mode prints ` + "`plan import:`" + `, ` + "`slug:`" + `, ` + "`title:`" + `,
` + "`tasks:`" + `, and ` + "`deviations:`" + ` lines; --json prints
` + "`{id, slug, title, source_slug, source_status, task_count, deviation_count, output}`" + `,
plus ` + "`declarations: {count, links, published, warnings}`" + ` for declaration
output.

## Exit codes

- ` + "`0`" + ` the plan (and deviations, if given) imported successfully
- ` + "`1`" + ` invalid invocation, or the control plane refused the plan as malformed
- ` + "`2`" + ` the plan/deviations file could not be read, or the control plane
  could not be reached
`
