package main

// `nodes chain-verify` (task t24, issue #45): the generic decompose-pipeline
// verification surface, exposed as an operable CLI verb rather than left as
// a Go-only library call. It fetches a run's ledger over the existing,
// read-only GET /v1alpha1/runs/{id}/ledger endpoint (no new API surface —
// t22's plan-import verb needed a new POST route because nothing read
// devague's shapes before it; this verb needs none, because
// internal/ledger.VerifyClaimChain reads only the generic envelope every run
// already exposes) and computes the chain verdict LOCALLY, the same way
// `nodes validate` compiles a workflow locally through internal/compiler
// rather than asking the control plane to do it.
//
// This is the "verification that computes a check" half of the ledger
// authority model (CLAUDE.md, PRD §10.4): the verdict this verb prints is a
// deterministic computation over already-committed records, exactly what
// `derived` authority means — Origin validator, Authority derived is the
// shape a caller wiring this verdict into the ledger would use. This verb
// itself never appends anything: like internal/devague's Map* functions, it
// is a pure read-and-compute path, and feeding its result through a real
// review/append transaction is a caller's decision, not this command's.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// ledgerRecordsResponse mirrors components.schemas.LedgerRecords
// (internal/api's LedgerRecordsOut) — the exact GET /v1alpha1/runs/{id}/ledger
// body, decoded straight into internal/ledger.Record so this verb's
// computation runs over the identical type VerifyClaimChain's own tests
// exercise, never a second hand-maintained shape.
type ledgerRecordsResponse struct {
	Items         []ledger.Record `json:"items"`
	LedgerVersion int64           `json:"ledger_version"`
}

// chainVerifyResultPayload is `nodes chain-verify --json`'s stable result
// shape.
type chainVerifyResultPayload struct {
	RunID     string                    `json:"run_id"`
	Passed    bool                      `json:"passed"`
	Claims    []ledger.ClaimSourceCheck `json:"claims"`
	Motivated []ledger.MotivatedCheck   `json:"motivated"`
}

// cmdChainVerify implements `nodes chain-verify`.
//
// Stream and exit contract, matching `nodes validate`'s: a failing chain is
// a DOMAIN outcome (an unsourced claim, an unmotivated decision), not an
// invocation error, so it is a result on stdout at exit 1 — never a
// CliError, never stderr. An unreachable control plane or an unknown run id
// stays an environment/user error respectively, exactly as every other
// verb's HTTP call reports them.
func cmdChainVerify(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("chain-verify")
	apiFlag := fs.String("api", "", "control-plane base URL (defaults to NODES_API_URL, then "+defaultAPIBaseURL+")")
	runFlag := fs.String("run", "", "the run id to verify (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			clifmt.EmitResult(explainChainVerify)
			return clifmt.ExitSuccess, nil
		}
		return 0, parseError("chain-verify", err)
	}
	if rest := fs.Args(); len(rest) != 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("chain-verify takes no positional arguments, got %q", rest),
			Remediation: "pass the run id as --run <id> — run 'nodes chain-verify --help' for the full flag list",
		}
	}
	if *runFlag == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "missing required flag: --run",
			Remediation: "pass --run <run-id> — run 'nodes chain-verify --help' for the full flag list",
		}
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
	records, cliErr := getRunLedger(client, baseURL, *runFlag)
	if cliErr != nil {
		return 0, cliErr
	}

	verdict := ledger.VerifyClaimChain(records)

	if err := emitChainVerifyResult(jsonMode, *runFlag, verdict); err != nil {
		return 0, err
	}
	if verdict.Passed {
		return clifmt.ExitSuccess, nil
	}
	return clifmt.ExitUserError, nil
}

// getRunLedger GETs the run's ledger feed and decodes it, translating the
// documented {code, message, remediation} error shape onto a CliError —
// run.go's apiErrorToCliError, reused so a missing run reads identically
// whichever verb hit it.
func getRunLedger(client *http.Client, baseURL, runID string) ([]ledger.Record, *clifmt.CliError) {
	resp, err := client.Get(baseURL + "/v1alpha1/runs/" + runID + "/ledger")
	if err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("cannot reach the control plane at %s: %v", baseURL, err),
			Remediation: "check that 'nodes serve' is running there, or point --api / NODES_API_URL at it",
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("read response from %s: %v", baseURL, err),
			Remediation: "check the connection to the control plane and try again",
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorToCliError(resp.StatusCode, data)
	}
	var out ledgerRecordsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("decode ledger response: %v", err),
			Remediation: "the control plane answered 200 with an unexpected body; check that its version matches this CLI",
		}
	}
	return out.Items, nil
}

// emitChainVerifyResult prints the verb's result: pass/fail plus every
// claim's sourcing check and every decision/task's motivation check.
func emitChainVerifyResult(jsonMode bool, runID string, verdict ledger.ChainVerification) error {
	if jsonMode {
		return clifmt.EmitResultJSON(chainVerifyResultPayload{
			RunID:     runID,
			Passed:    verdict.Passed,
			Claims:    nonNilClaimChecks(verdict.Claims),
			Motivated: nonNilMotivatedChecks(verdict.Motivated),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "chain-verify: %s\npassed: %t\n", runID, verdict.Passed)
	for _, c := range verdict.Claims {
		fmt.Fprintf(&b, "claim %s: sourced=%t sources=%d\n", c.ClaimID, c.Sourced, c.SourceCount)
	}
	for _, m := range verdict.Motivated {
		fmt.Fprintf(&b, "%s %s: motivated=%t claim_refs=%s\n", m.RecordType, m.RecordID, m.Motivated, strings.Join(m.ClaimRefs, ","))
	}
	clifmt.EmitResult(strings.TrimRight(b.String(), "\n"))
	return nil
}

func nonNilClaimChecks(c []ledger.ClaimSourceCheck) []ledger.ClaimSourceCheck {
	if c == nil {
		return []ledger.ClaimSourceCheck{}
	}
	return c
}

func nonNilMotivatedChecks(m []ledger.MotivatedCheck) []ledger.MotivatedCheck {
	if m == nil {
		return []ledger.MotivatedCheck{}
	}
	return m
}

// explainChainVerify is the `nodes explain chain-verify` entry and the
// `nodes chain-verify --help` text.
const explainChainVerify = `# nodes chain-verify

Checks a run's decompose chain (task t24, issue #45): fetches
` + "`GET /v1alpha1/runs/{id}/ledger`" + ` and reports, for every live claim,
whether it carries a source, and for every live decision or task, whether it
traces back to a motivating claim through its own provenance. This is the
generic verification surface the decompose pipeline ends on — document to
claims (with sources) to connected decisions and actions to verified in the
end — the same function whether the run's claims came from a devague plan
import or a web-scoped newsletter (examples/newsletter-decompose).

The verdict is computed locally, like ` + "`nodes validate`" + ` compiles a
workflow locally: this verb introduces no new API route and appends nothing
to the ledger itself.

## Usage

    nodes chain-verify --run run_01J...
    nodes chain-verify --run run_01J... --json

## Flags

- ` + "`--run`" + ` (required) — the run id to verify.
- ` + "`--api`" + ` — control-plane base URL (default: NODES_API_URL, then
  ` + defaultAPIBaseURL + `).

## Output

Text mode prints ` + "`chain-verify:`" + `, ` + "`passed:`" + `, one
` + "`claim ...`" + ` line per live claim, and one
` + "`<record_type> ... motivated=...`" + ` line per live decision/task;
--json prints ` + "`{run_id, passed, claims, motivated}`" + `.

## Exit codes

- ` + "`0`" + ` every claim is sourced and every decision/task is motivated
- ` + "`1`" + ` invalid invocation, the chain failed (a domain outcome: an
  unsourced claim or an unmotivated decision/task), or the control plane
  refused the run id (e.g. not found)
- ` + "`2`" + ` the control plane could not be reached, or answered
  something this CLI cannot parse
`
