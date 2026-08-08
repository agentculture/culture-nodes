package runners

import "context"

// Runner executes one typed Operation behind an enforced policy boundary.
//
// It is deliberately a single method taking a document and returning a
// document. There is no "run this command" method, no io.Writer for a shell,
// and no escape hatch: everything an adapter is allowed to do is expressible
// as Operation, and everything it is allowed to claim is expressible as
// Result.
//
// # Error contract
//
// Execute returns a Result and a nil error when an execution actually
// happened and the adapter can honestly report on it — including when the
// executed process failed, timed out, or exited nonzero. Those are results,
// not dispatch errors.
//
// Execute returns the zero Result and a *DispatchError when no execution
// happened, or when the adapter cannot honestly say whether one did: a
// refused operation (unregistered identity, oversize payload, a timeout the
// platform cannot honour), a throttle, an auth failure, a transport error.
// Returning a fabricated "failed" Result for those cases would put an
// unmeasured claim into the ledger, so the adapter declines to make one.
//
// Callers classify a *DispatchError with errors.As and read Kind/Retryable;
// DispatchError.TechStatus maps it onto the engine's technical statuses.
type Runner interface {
	Execute(ctx context.Context, op Operation) (Result, error)
}
