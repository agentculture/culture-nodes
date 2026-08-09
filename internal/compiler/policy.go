package compiler

import (
	"fmt"
	"strings"
	"time"
)

// digestPin is the marker that says a component or image reference is pinned
// to immutable content rather than to a mutable tag (PRD §9.10, §13.7).
const digestPin = "@sha256:"

// checkPolicy is the §11.4 policy level: durations parse and are positive,
// retry policies are sane, components are pinned, and no code node asks the
// runner for more than the runner can give.
func (c *compilation) checkPolicy() {
	if c.doc.Spec.Limits != nil {
		c.checkDuration("/spec/limits/maxDuration", "spec.limits.maxDuration", c.doc.Spec.Limits.MaxDuration)
	}

	for _, id := range c.nodeIDs {
		n := c.doc.Spec.Nodes[id]
		base := pointerJoin("/spec/nodes", id)

		if n.Policy != nil {
			c.checkDuration(base+"/policy/timeout", fmt.Sprintf("node %q timeout", id), n.Policy.Timeout)
			c.checkRetry(base, id, n.Policy.Retry)
		}
		c.checkDuration(base+"/deadline", fmt.Sprintf("node %q deadline", id), n.Deadline)
		if n.Until != nil {
			c.checkDuration(base+"/until/duration", fmt.Sprintf("node %q wait duration", id), n.Until.Duration)
		}

		if n.Uses != "" && !strings.Contains(n.Uses, digestPin) {
			c.add(LevelWarning, base+"/uses", CodePolicyComponentUnpinned,
				fmt.Sprintf("component reference %q is not pinned to a digest", n.Uses),
				"append @sha256:<digest>; production runs never resolve mutable tags at execution time (PRD §9.10)")
		}

		c.checkOperation(base, id, n)
		c.checkRunnerCaps(base, id, n)
		c.checkHookOperations(base, id, n)
	}
}

func (c *compilation) checkDuration(path, subject, value string) {
	if value == "" {
		return
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		c.add(LevelError, path, CodePolicyDurationInvalid,
			fmt.Sprintf("%s: %q is not a duration", subject, value),
			"use a Go duration literal such as 900ms, 5m, 2h, or 1h30m")
		return
	}
	if parsed <= 0 {
		c.add(LevelError, path, CodePolicyDurationInvalid,
			fmt.Sprintf("%s: %q is not positive", subject, value),
			"a zero or negative bound would abort the work it is meant to bound; give it a positive duration")
	}
}

func (c *compilation) checkRetry(base, id string, retry *retryPolicy) {
	if retry == nil || retry.MaxAttempts == nil {
		return
	}
	if *retry.MaxAttempts > MaxRetryAttempts {
		c.add(LevelError, base+"/policy/retry/maxAttempts", CodePolicyRetryExcessive,
			fmt.Sprintf("node %q requests %d attempts; the maximum is %d", id, *retry.MaxAttempts, MaxRetryAttempts),
			fmt.Sprintf("lower maxAttempts to at most %d, or make the node's work resumable instead of repeated", MaxRetryAttempts))
	}
}

func (c *compilation) checkOperation(base, id string, n *node) {
	c.checkOperationPolicy(base+"/operation", id, "operation", n.Operation)
}

// checkOperationPolicy applies the image-pin and requires-shell safe-default
// checks any declared code operation gets. It is shared between a code node's
// own operation and a pre_run/post_run hook's operation (task t14): both
// execute through the same runner boundary (PRD §13.7), so the same
// safe-default checks apply to both — "hook operations validate like
// code-node operations".
func (c *compilation) checkOperationPolicy(path, id, subject string, op *codeOperation) {
	if op == nil {
		return
	}
	if op.Image != "" && !strings.Contains(op.Image, digestPin) {
		c.add(LevelWarning, path+"/image", CodePolicyImageUnpinned,
			fmt.Sprintf("node %q %s runs image %q, which is not pinned to a digest", id, subject, op.Image),
			"pin the image as registry/name@sha256:<digest> (PRD §13.7 safe defaults)")
	}
	if op.RequiresShell != nil && *op.RequiresShell {
		c.add(LevelWarning, path+"/requiresShell", CodePolicyShellRequested,
			fmt.Sprintf("node %q %s declares that it requires a shell", id, subject),
			"prefer an argv array; the declaration is honoured so policy can reject it, and a policy set may (PRD §13.7)")
	}
}

// checkHookOperations applies the same operation-shape policy checks to a
// node's pre_run/post_run hook operations. Unlike a code node's own
// operation, a hook has no policy block of its own in this schema slice
// (task t14 reuses the code node's operation shape verbatim, and adds no
// per-hook timeout or payload override) — so there is no author-declared
// hook timeout or payload bound to check against the runner's caps, and only
// the image-pin/requires-shell checks apply here. Whether the node is a kind
// that may legally declare a hook at all is checkNodeHooks's job (§11.4
// contract level), not this one's.
func (c *compilation) checkHookOperations(base, id string, n *node) {
	if n.PreRun != nil {
		c.checkOperationPolicy(base+"/pre_run/operation", id, "pre_run hook", &n.PreRun.Operation)
	}
	if n.PostRun != nil {
		c.checkOperationPolicy(base+"/post_run/operation", id, "post_run hook", &n.PostRun.Operation)
	}
}

// checkRunnerCaps refuses a code node the runner could not execute. The
// diagnostic names both the cap and where the cap comes from, because
// "rejected" without a number is not something an author can act on.
func (c *compilation) checkRunnerCaps(base, id string, n *node) {
	if n.Kind != KindCode {
		return
	}

	if timeout := effectiveTimeout(n); timeout != "" {
		if parsed, err := time.ParseDuration(timeout); err == nil {
			if seconds := int(parsed.Seconds()); seconds > RunnerMaxTimeoutSeconds {
				c.add(LevelError, base+"/policy/timeout", CodePolicyTimeoutOverCap,
					fmt.Sprintf("code node %q has timeout %s (%ds), above the maximum operation timeout of %ds (limit source: %s)",
						id, timeout, seconds, RunnerMaxTimeoutSeconds, RunnerLimitSource),
					fmt.Sprintf("lower the timeout to at most %ds, or split the operation so each part fits the runner's limit", RunnerMaxTimeoutSeconds))
			}
		}
	}

	if n.Contract != nil && n.Contract.MaxInlinePayloadBytes != nil {
		if declared := *n.Contract.MaxInlinePayloadBytes; declared > RunnerMaxPayloadBytes {
			c.add(LevelError, base+"/contract/maxInlinePayloadBytes", CodePolicyPayloadOverCap,
				fmt.Sprintf("code node %q declares an inline payload of %d bytes, above the maximum of %d bytes (limit source: %s)",
					id, declared, RunnerMaxPayloadBytes, RunnerLimitSource),
				fmt.Sprintf("lower maxInlinePayloadBytes to at most %d, and move larger data through artifact references", RunnerMaxPayloadBytes))
		}
	}
}
