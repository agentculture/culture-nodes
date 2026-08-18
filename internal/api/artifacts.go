package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/artifacts"
	postgres "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// MaxArtifactBytes bounds a single artifact publication.
//
// Chosen as the largest thing this route is FOR -- a run's context bundle, a
// diff, a log, a report -- and deliberately not large enough to be a general
// file transfer. The two-carrier decision (spec q9/c70) sends a runner's
// CHANGES as a git ref, not as an artifact, so nothing legitimate on this path
// should approach it. A limit that is never hit in normal use is the point:
// when it fires, something is wrong, and the caller is told which.
const MaxArtifactBytes = 64 << 20 // 64 MiB

type artifactInvocationStore interface {
	Invocation(context.Context, string) (actors.PendingInvocation, error)
}

// artifactRunnerOpSource is the fallback association source for RUNNER
// attempts: the runner_operations rows recorded at dispatch. Implemented by
// *postgres.Store.
type artifactRunnerOpSource interface {
	ListRunnerOperationsByAttempt(ctx context.Context, attemptID string) ([]postgres.RunnerOperationRecord, error)
}

type artifactWriteResponse struct {
	Ref       artifacts.Ref     `json:"ref"`
	Name      string            `json:"name"`
	MediaType string            `json:"media_type"`
	SizeBytes int64             `json:"size_bytes"`
	Digest    string            `json:"digest"`
	Backend   artifacts.Backend `json:"backend"`
	CreatedAt string            `json:"created_at"`
}

func (s *Server) handlePutArtifact(w http.ResponseWriter, r *http.Request) error {
	attemptID := r.PathValue("attemptID")
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return unauthorized("present the attempt callback token as Authorization: Bearer <token>", "artifact publication requires a bearer token")
	}
	if s.callbackSigner == nil || s.artifactRouter == nil || s.artifactInvocationStore == nil {
		return unavailable("configure the callback signer and artifact router", "artifact publication is not configured")
	}

	// VerifyFor binds the credential to the path before any durable lookup or
	// body read. The attempt id never comes from artifact content.
	if err := s.callbackSigner.VerifyFor(token, attemptID); err != nil {
		return unauthorized("use the unexpired callback token issued for the attempt named in the path", "artifact callback token rejected")
	}

	inv, err := s.artifactInvocationStore.Invocation(r.Context(), attemptID)
	if err != nil {
		if !errors.Is(err, actors.ErrUnknownAttempt) {
			return internalError(err)
		}
		// Not an async ACTOR attempt. A RUNNER attempt is just as durable —
		// its runner_operations row is written at dispatch, before the runner
		// could possibly hold this attempt's callback token — so resolve the
		// associations there (deviation d1's production gap, found live: the
		// green 18:10Z sweep's stdout upload 404'd here because only actor
		// attempts have pending invocations).
		inv, err = s.runnerAttemptAssociations(r.Context(), attemptID)
		if err != nil {
			if errors.Is(err, actors.ErrUnknownAttempt) {
				return notFound("check that the attempt is a durable pending invocation or a dispatched runner operation", "artifact attempt not found")
			}
			return internalError(err)
		}
	}
	if inv.NamespaceID == "" || inv.AttemptID == "" || inv.AttemptID != attemptID {
		return internalError(errors.New("artifact publication: durable invocation lacks required namespace or matching attempt association"))
	}

	mediaType := strings.TrimSpace(r.Header.Get("Content-Type"))
	name := strings.TrimSpace(r.Header.Get("Artifact-Name"))
	if mediaType == "" || name == "" {
		return badRequest("set both Content-Type and Artifact-Name headers", "artifact media type and descriptive name are required")
	}

	// Neither artifacts.Store nor its postgres/s3 drivers bound what they will
	// accept -- they stream whatever the reader yields. Authentication is not a
	// size limit: a legitimately dispatched attempt with a runaway loop, or one
	// whose token leaked, could otherwise stream until the volume fills. Bound
	// it here, at the only place that knows this is an untrusted network body.
	body := http.MaxBytesReader(w, r.Body, MaxArtifactBytes)
	ref, err := s.artifactRouter.Put(r.Context(), artifacts.ArtifactMeta{
		NamespaceID: inv.NamespaceID,
		RunID:       inv.RunID,
		AttemptID:   inv.AttemptID,
		Name:        name,
		MediaType:   mediaType,
	}, body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return payloadTooLarge(
				"publish large output to object storage and record a reference, or split it",
				"artifact body exceeds the publication limit")
		}
		return internalError(err)
	}
	meta, err := s.artifactRouter.Stat(r.Context(), ref)
	if err != nil {
		return internalError(err)
	}

	writeJSON(w, http.StatusCreated, artifactWriteResponse{
		Ref:       ref,
		Name:      meta.Name,
		MediaType: meta.MediaType,
		SizeBytes: meta.SizeBytes,
		Digest:    meta.Digest,
		Backend:   meta.Backend,
		CreatedAt: meta.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	})
	return nil
}

// bearerToken extracts the credential from an Authorization header.
//
// The scheme is matched case-insensitively, like every other authenticated
// route in this package (see actors.go, humantasks.go, adhoc.go,
// signalevents.go) and like RFC 7235 requires. A case-sensitive match here
// would reject a spec-compliant client that sent `bearer ` and give it a 401
// naming the token as the problem, which is the least debuggable failure this
// route can produce.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// artifactListEntry is one row of the attempt-artifact listing: the same
// fields the write response reports, so a reader can correlate what a PUT
// returned with what the listing now shows.
type artifactListEntry struct {
	Ref       artifacts.Ref     `json:"ref"`
	Name      string            `json:"name"`
	MediaType string            `json:"media_type"`
	SizeBytes int64             `json:"size_bytes"`
	Digest    string            `json:"digest"`
	Backend   artifacts.Backend `json:"backend"`
	CreatedAt string            `json:"created_at"`
}

// handleListAttemptArtifacts is the read-back half of the artifact route
// (issue #189): GET /v1alpha1/attempts/{attemptID}/artifacts lists what the
// attempt published. Like the other read surfaces in this package it carries
// no bearer token -- attempt ids are store-minted ULIDs, and the listing is
// exactly what the run's ledger evidence already references by ref.
func (s *Server) handleListAttemptArtifacts(w http.ResponseWriter, r *http.Request) error {
	attemptID := r.PathValue("attemptID")
	if attemptID == "" {
		return badRequest("name the attempt in the path", "attempt id is required")
	}
	if s.artifactRouter == nil {
		return unavailable("configure the artifact router", "artifact reads are not configured")
	}
	listed, err := s.artifactRouter.ListByAttempt(r.Context(), attemptID)
	if err != nil {
		return internalError(err)
	}
	out := make([]artifactListEntry, 0, len(listed))
	for _, l := range listed {
		out = append(out, artifactListEntry{
			Ref:       l.Ref,
			Name:      l.Meta.Name,
			MediaType: l.Meta.MediaType,
			SizeBytes: l.Meta.SizeBytes,
			Digest:    l.Meta.Digest,
			Backend:   l.Meta.Backend,
			CreatedAt: l.Meta.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempt_id": attemptID, "artifacts": out})
	return nil
}

// handleGetAttemptArtifact streams one named artifact's content:
// GET /v1alpha1/attempts/{attemptID}/artifacts/{name}. The name is the
// descriptive Artifact-Name recorded at PUT time ("stdout" for the runner's
// captured output); when an attempt recorded several artifacts under one
// name, the newest wins -- same row order the listing shows.
func (s *Server) handleGetAttemptArtifact(w http.ResponseWriter, r *http.Request) error {
	attemptID := r.PathValue("attemptID")
	name := r.PathValue("name")
	if attemptID == "" || name == "" {
		return badRequest("name both the attempt and the artifact in the path", "attempt id and artifact name are required")
	}
	if s.artifactRouter == nil {
		return unavailable("configure the artifact router", "artifact reads are not configured")
	}
	listed, err := s.artifactRouter.ListByAttempt(r.Context(), attemptID)
	if err != nil {
		return internalError(err)
	}
	var match *artifacts.Listed
	for i := range listed {
		if listed[i].Meta.Name == name {
			match = &listed[i] // keep scanning: newest (last, by created_at order) wins
		}
	}
	if match == nil {
		return notFound("list the attempt's artifacts to see recorded names", "attempt %s has no artifact named %q", attemptID, name)
	}
	content, meta, err := s.artifactRouter.Get(r.Context(), match.Ref)
	if err != nil {
		var reaped *artifacts.ReapedError
		if errors.As(err, &reaped) {
			return notFound("the artifact was reaped: "+reaped.Tombstone.Reason, "artifact %s content was reaped", match.Ref)
		}
		return internalError(err)
	}
	defer content.Close()
	if meta.MediaType != "" {
		w.Header().Set("Content-Type", meta.MediaType)
	}
	w.Header().Set("Artifact-Ref", string(match.Ref))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, content); err != nil {
		// Headers are gone; all we can do is stop. The verifying reader has
		// already surfaced size/digest mismatches as read errors here.
		return nil
	}
	return nil
}

// runnerAttemptAssociations resolves a RUNNER attempt's durable associations
// from its runner_operations row (written at dispatch), shaped as the same
// PendingInvocation the actor path yields so handlePutArtifact treats both
// attempt kinds identically. The run id rides inside the recorded operation's
// context; a row without one still associates namespace and attempt — RunID
// stays optional metadata on the artifact row, exactly as ArtifactMeta
// documents.
func (s *Server) runnerAttemptAssociations(ctx context.Context, attemptID string) (actors.PendingInvocation, error) {
	if s.artifactRunnerOps == nil {
		return actors.PendingInvocation{}, actors.ErrUnknownAttempt
	}
	ops, err := s.artifactRunnerOps.ListRunnerOperationsByAttempt(ctx, attemptID)
	if err != nil {
		return actors.PendingInvocation{}, err
	}
	if len(ops) == 0 {
		return actors.PendingInvocation{}, actors.ErrUnknownAttempt
	}
	var req struct {
		Context struct {
			RunID string `json:"run_id"`
		} `json:"context"`
	}
	_ = json.Unmarshal(ops[0].Request, &req) // absent context stays empty, not an error
	return actors.PendingInvocation{
		NamespaceID: ops[0].NamespaceID,
		RunID:       req.Context.RunID,
		AttemptID:   attemptID,
	}, nil
}
