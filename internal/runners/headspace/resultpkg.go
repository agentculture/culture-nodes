package headspace

import (
	"encoding/json"
	"fmt"
)

// This file is a Go mirror of headspace-cli's own result document, read from
// /home/spark/git/headspace-cli/headspace/core/result.py (the nine-section
// context-return package) and headspace/cli/_errors.py (the CliError shape
// every non-zero-exit-1/2/3/7 failure emits on stderr). It is a read-only
// reference, not a dependency: headspace-cli is a separate process reached
// only by subprocess, and these structs exist so this package can parse the
// single line of JSON that process writes to stdout or stderr.
//
// Field coverage is deliberately partial. resultPackage carries every field
// this bridge actually reads; a future headspace-cli release that adds a
// tenth section decodes here without error (encoding/json ignores unknown
// keys) and simply isn't consulted until someone asks for it.

// The status vocabulary headspace-cli's result package uses, mirrored from
// result.py's STATUSES tuple. Only success/failure/timeout/cancelled and
// resource_exhausted are statuses this bridge's run step can actually observe
// (create/put/export/destroy always report success when they exit 0);
// partial_success, policy_denied, and infrastructure_failure are part of the
// vocabulary but not statuses headspace-cli's own CLI verbs emit today.
const (
	statusSuccess           = "success"
	statusFailure           = "failure"
	statusTimeout           = "timeout"
	statusCancelled         = "cancelled"
	statusResourceExhausted = "resource_exhausted"
)

// resultPackage is the nine-section document every headspace-cli verb prints
// as one line of JSON to stdout when it exits 0, 4, 5, 6, or 8 (see
// classifyVerbExit). Field names mirror result.py's ResultPackage.to_dict
// exactly.
type resultPackage struct {
	OutcomeSummary string         `json:"outcome_summary"`
	Status         string         `json:"status"`
	KeyFindings    []string       `json:"key_findings"`
	Evidence       []evidenceItem `json:"evidence"`
	Artifacts      []artifactItem `json:"artifacts"`
	Warnings       []string       `json:"warnings"`
	ResourceUsage  resourceUsage  `json:"resource_usage"`
	Provenance     provenance     `json:"provenance"`
	Attention      []string       `json:"attention"`
}

// evidenceItem mirrors result.py's Evidence.to_dict. "captured output" is the
// label headspace-cli's run/inspect verbs use for the one excerpt this bridge
// reads (see runExcerpt).
type evidenceItem struct {
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Truncated bool   `json:"truncated"`
	Excerpt   string `json:"excerpt"`
}

// artifactItem mirrors result.py's Artifact.to_dict, as returned by `export`.
type artifactItem struct {
	Name      string `json:"name"`
	Purpose   string `json:"purpose"`
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	Reference string `json:"reference"`
}

// resourceUsage mirrors result.py's ResourceUsage.to_dict. MaxMemoryBasis
// states whether MaxMemoryBytes is a sampled peak or (on a resource_exhausted
// result) a sampled floor -- see buildObservations for why that distinction
// is preserved rather than collapsed.
type resourceUsage struct {
	WallTimeSeconds float64 `json:"wall_time_seconds"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	MaxMemoryBytes  int64   `json:"max_memory_bytes"`
	MaxMemoryBasis  string  `json:"max_memory_basis"`
	StorageBytes    int64   `json:"storage_bytes"`
	OutputBytes     int64   `json:"output_bytes"`
}

// provenance mirrors result.py's Provenance.to_dict. These are the four
// fields task t18 names explicitly: workspace_id, job_id, image_digest, and
// policy_summary, all reported by headspace-cli itself.
type provenance struct {
	WorkspaceID   string   `json:"workspace_id"`
	JobID         string   `json:"job_id"`
	Profile       string   `json:"profile"`
	ImageDigest   string   `json:"image_digest"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	PolicySummary string   `json:"policy_summary"`
	Inputs        []string `json:"inputs"`
	TraceID       string   `json:"trace_id"`
}

// cliError mirrors headspace/cli/_errors.py's CliError.to_dict -- the shape
// every exit-1/2/3/7 failure writes to stderr as one line of JSON.
type cliError struct {
	Code        int    `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	Category    string `json:"category"`
}

// parseResultPackage decodes stdout as a resultPackage. headspace-cli always
// writes exactly one line (json.dump followed by "\n"); this bridge does not
// trim trailing content because there should be none, and a decode failure on
// unexpected trailing bytes is itself informative.
func parseResultPackage(stdout []byte) (*resultPackage, error) {
	var pkg resultPackage
	if err := json.Unmarshal(stdout, &pkg); err != nil {
		return nil, fmt.Errorf("decode result package: %w", err)
	}
	return &pkg, nil
}

// parseCLIError decodes stderr as a cliError.
func parseCLIError(stderr []byte) (*cliError, error) {
	var ce cliError
	if err := json.Unmarshal(stderr, &ce); err != nil {
		return nil, fmt.Errorf("decode CLI error: %w", err)
	}
	return &ce, nil
}
