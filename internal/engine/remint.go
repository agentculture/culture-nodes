package engine

// RemintableTechnicalFailure classifies completion semantics, rather than
// naming any particular domain outcome. Domain outcomes are answers: every
// successful completion is therefore excluded, regardless of its outcome
// vocabulary. Only a terminal technical failure without an answer may cause
// the control plane to re-mint a trigger-created run.
func RemintableTechnicalFailure(status TechStatus, outcome string) bool {
	if outcome != "" || status == StatusSucceeded {
		return false
	}
	switch status {
	case StatusFailed, StatusTimedOut, StatusCancelled, StatusPolicyDenied, StatusContractRejected:
		return true
	default:
		return false
	}
}
