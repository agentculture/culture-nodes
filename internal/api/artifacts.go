package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/artifacts"
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
		if errors.Is(err, actors.ErrUnknownAttempt) {
			return notFound("check that the attempt is a durable pending invocation", "artifact attempt not found")
		}
		return internalError(err)
	}
	if inv.NamespaceID == "" || inv.RunID == "" || inv.AttemptID == "" || inv.AttemptID != attemptID {
		return internalError(errors.New("artifact publication: durable invocation lacks required namespace, run, or matching attempt association"))
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
