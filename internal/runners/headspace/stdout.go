package headspace

import (
	"bytes"
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// This file is the durable stdout capture (issue #189): a green code-node
// run used to discard what the process printed -- the "captured output"
// excerpt fed the logs observation and was then lost when Execute's cleanup
// destroyed the workspace. When BridgeConfig carries an ArtifactStore, the
// excerpt is stored through it as an attempt-tied artifact and referenced
// from Result.Artifacts.StdoutRef, so one query against the attempt's
// artifacts can distinguish a process that printed {"emitted": 0} from one
// that printed {"emitted": 7} -- and both from one that printed nothing
// (an empty capture is still stored: a zero-byte artifact with a resolvable
// ref IS the durable statement "it printed nothing").

// obsStdoutArtifact is the Observations.Additional key the stored-stdout
// outcome is recorded under, in every store-configured case -- stored,
// store-write failed, or nothing to store -- so the field's absence always
// means "no store was configured", never "the bridge forgot to say".
const obsStdoutArtifact = "stdout_artifact"

// stdoutArtifactName / stdoutArtifactMediaType are the descriptive metadata
// recorded on the stored artifact. "stdout" mirrors runners.Artifacts'
// stdout_ref vocabulary (and is ArtifactMeta.Name's own doc example).
const (
	stdoutArtifactName      = "stdout"
	stdoutArtifactMediaType = "text/plain; charset=utf-8"
)

// maxStdoutArtifactBytes caps the stored stdout capture at 64 KiB. There is
// no repo-wide truncation convention at this size to follow -- this
// package's maxDetailBytes (2 KiB) bounds error-message quoting, and the
// API's MaxArtifactBytes (64 MiB) bounds an untrusted network body; neither
// is a payload cap for a capture this bridge itself composes -- so the task
// brief's 64 KiB default applies. In practice the excerpt is already
// bounded well below this by headspace-cli's own ~8 KiB context-return
// budget; the cap is a defensive ceiling against a future headspace-cli
// raising that budget, not a bound this code path expects to hit. A capped
// capture is stored anyway and declared incomplete on the stdout_artifact
// observation.
const maxStdoutArtifactBytes = 64 << 10

// storeCapturedStdout persists pkg's captured-output excerpt through
// b.artifactStore and records the outcome on result -- the ref on
// Artifacts.StdoutRef and the honesty on the stdout_artifact observation.
// A nil store is a no-op: the pre-existing local-only behaviour, chosen at
// New time, not silently half-claimed here.
//
// The store write runs under a cancellation-detached, StopTimeout-bounded
// context, for the same reason cleanupWorkspace's does: on a cancelled or
// timed-out run, parent may be done precisely when the Result -- which this
// capture is part of -- still has to be assembled and returned.
//
// A store failure never turns a genuine execution result into a dispatch
// failure: the run happened and this bridge can honestly describe it; the
// failed write is recorded on the observation instead, exactly the posture
// exportDeclared takes for a failed export.
func (b *Bridge) storeCapturedStdout(parent context.Context, op runners.Operation, pkg *resultPackage, result *runners.Result) {
	if b.artifactStore == nil {
		return
	}
	if result.Observations.Additional == nil {
		result.Observations.Additional = map[string]runners.Observation{}
	}

	excerpt, truncatedByCLI, ok := runExcerpt(pkg)
	if !ok {
		result.Observations.Additional[obsStdoutArtifact] = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_run_evidence_excerpt_stored",
			Scope:    "None.",
			Note: "The run result carried no captured-output evidence entry, so there is no stdout capture to " +
				"store -- distinct from an empty capture, which IS stored as a zero-byte artifact.",
		}
		return
	}

	data := []byte(excerpt)
	capped := false
	if len(data) > maxStdoutArtifactBytes {
		data = data[:maxStdoutArtifactBytes]
		capped = true
	}

	meta := artifacts.ArtifactMeta{
		NamespaceID: b.artifactNamespace,
		Name:        stdoutArtifactName,
		MediaType:   stdoutArtifactMediaType,
	}
	if op.Context != nil {
		meta.RunID = op.Context.RunID
		meta.AttemptID = op.Context.AttemptID
	}

	ctx, cancel := context.WithTimeout(detachedContext(parent), b.stopTimeout)
	defer cancel()
	ref, putErr := b.artifactStore.Put(ctx, meta, bytes.NewReader(data))
	if putErr != nil {
		result.Observations.Additional[obsStdoutArtifact] = runners.Observation{
			Measured: false,
			Complete: false,
			Method:   "headspace_run_evidence_excerpt_stored",
			Scope:    "None.",
			Note: fmt.Sprintf(
				"Storing the captured stdout failed: %v -- the run itself genuinely happened and its Result stands; "+
					"only the durable copy of what the process printed is missing.", putErr),
		}
		return
	}

	if result.Artifacts == nil {
		result.Artifacts = &runners.Artifacts{}
	}
	result.Artifacts.StdoutRef = string(ref)

	obs := runners.Observation{
		Measured: true,
		Complete: !truncatedByCLI && !capped,
		Method:   "headspace_run_evidence_excerpt_stored",
		Scope: fmt.Sprintf(
			"The run's captured-output excerpt (%d bytes), stored durably as %s and referenced from artifacts.stdout_ref.",
			len(data), ref),
		Note: "headspace-cli captured the process's output; this bridge stored its excerpt of that capture. " +
			"An empty artifact means the process printed nothing.",
	}
	switch {
	case capped:
		obs.Note = fmt.Sprintf(
			"The capture exceeded this bridge's %d-byte stored-stdout cap and was stored truncated to it; "+
				"the stored copy is a prefix of what the process printed.", maxStdoutArtifactBytes)
	case truncatedByCLI:
		obs.Note = "headspace-cli itself truncated the excerpt under its own context-return byte budget, so the " +
			"stored copy is a prefix of what the process printed."
	}
	result.Observations.Additional[obsStdoutArtifact] = obs
}
