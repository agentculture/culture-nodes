package runners

import (
	"errors"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// Sentinel errors for the refusals a caller may want to match without
// inspecting a DispatchError's fields. Every one of them is also reachable as
// a *DispatchError carrying a Kind, so errors.Is and errors.As both work.
var (
	// ErrUnregisteredFunction is dispatch to an identity the registry does
	// not hold. This is spec claim c41's core refusal: the control plane can
	// never invoke an arbitrary function, and the refusal happens before any
	// call leaves the process.
	ErrUnregisteredFunction = errors.New("runners: function identity is not registered")
	// ErrDigestMismatch is an operation whose pinned image digest disagrees
	// with the digest the registry recorded for that identity. Pinning that
	// is not checked is not pinning.
	ErrDigestMismatch = errors.New("runners: operation image digest does not match the registered digest")
	// ErrOversizePayload is a request payload too large for the platform's
	// synchronous transport. The remedy is an artifact ref, never a
	// truncated payload.
	ErrOversizePayload = errors.New("runners: operation payload exceeds the runner's synchronous request limit")
	// ErrTimeoutNotEnforceable is a policy timeout the platform cannot
	// honour. An adapter that cannot enforce a limit refuses the operation
	// rather than silently running under a different one.
	ErrTimeoutNotEnforceable = errors.New("runners: policy timeout exceeds what the runner can enforce")
	// ErrUnsupportedOperation is an operation this adapter does not
	// implement — a container execution handed to a function adapter, a
	// policy field it cannot enforce.
	ErrUnsupportedOperation = errors.New("runners: operation is not one this runner implements")
	// ErrRunnerUnavailable is a platform-side failure to dispatch at all.
	ErrRunnerUnavailable = errors.New("runners: runner is unavailable")
	// ErrAccessDenied is the platform refusing the dispatch on identity or
	// policy grounds. It is deliberately distinct from
	// ErrUnregisteredFunction: "this process declined to ask" and "the
	// platform said no" are different facts, and conflating them would hide
	// which of the two boundaries actually held.
	ErrAccessDenied = errors.New("runners: the runner platform denied the request")
	// ErrRateLimited is the platform throttling the dispatch.
	ErrRateLimited = errors.New("runners: the runner platform throttled the request")
	// ErrTransport is a network or protocol failure reaching the runner.
	ErrTransport = errors.New("runners: transport failure reaching the runner")
)

// SentinelFor returns the sentinel that matches a failure kind, so errors.Is
// on a classified platform failure says something true rather than something
// merely convenient. Kinds with no sentinel of their own return nil, which is
// an honest "this one is classified by Kind alone".
func SentinelFor(kind ErrorKind) error {
	switch kind {
	case ErrorRateLimited:
		return ErrRateLimited
	case ErrorAuthOrPolicy:
		return ErrAccessDenied
	case ErrorRunnerUnavailable:
		return ErrRunnerUnavailable
	case ErrorRetryableTransport:
		return ErrTransport
	case ErrorRejectedInput:
		return ErrUnsupportedOperation
	default:
		return nil
	}
}

// DispatchError is a refusal or a transport failure: the adapter did not
// produce a Result because no execution it could honestly describe took
// place. Kind mirrors the result schema's error kinds so a caller that
// records the failure uses the same vocabulary either way.
type DispatchError struct {
	// Kind classifies the failure using the result schema's enum.
	Kind ErrorKind
	// OperationID is the operation that was refused, when one was named.
	OperationID string
	// Identity is the function identity involved, when one was named. It is
	// the *requested* name, which for an unregistered dispatch is the whole
	// point of the message.
	Identity string
	// Detail is the human-readable reason, including the remediation when
	// there is one (pass S3 refs; lower the timeout; register the function).
	Detail string
	// Err is the sentinel this refusal matches, so errors.Is works.
	Err error
}

// Error renders the refusal with its operation and identity when known.
func (e *DispatchError) Error() string {
	msg := string(e.Kind)
	if e.OperationID != "" {
		msg += " for operation " + e.OperationID
	}
	if e.Identity != "" {
		msg += " (identity " + e.Identity + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		return fmt.Sprintf("%v: %s", e.Err, msg)
	}
	return "runners: " + msg
}

// Unwrap exposes the sentinel so errors.Is matches it.
func (e *DispatchError) Unwrap() error { return e.Err }

// Retryable reports whether the runtime may retry this dispatch on its own.
func (e *DispatchError) Retryable() bool { return e.Kind.Retryable() }

// TechStatus maps a dispatch failure onto the engine's technical statuses
// (PRD §3.4). None of these are domain outcomes: a refused dispatch produced
// no domain answer at all, so there is nothing for an edge to route on.
func (e *DispatchError) TechStatus() engine.TechStatus {
	switch e.Kind {
	case ErrorAuthOrPolicy:
		return engine.StatusPolicyDenied
	case ErrorRejectedInput, ErrorContractFailure:
		return engine.StatusContractRejected
	case ErrorTimeout:
		return engine.StatusTimedOut
	case ErrorCancellation:
		return engine.StatusCancelled
	default:
		return engine.StatusFailed
	}
}

// refuse builds a DispatchError. It exists so every refusal in this package
// and its adapters is shaped identically and carries a remediation.
func refuse(kind ErrorKind, sentinel error, operationID, identity, detail string) *DispatchError {
	return &DispatchError{
		Kind:        kind,
		OperationID: operationID,
		Identity:    identity,
		Detail:      detail,
		Err:         sentinel,
	}
}
