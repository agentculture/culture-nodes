package actors_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

type dialInStub struct {
	response actors.InvocationResponse
	err      error
	calls    int
}

func (s *dialInStub) InvokeInbound(context.Context, string, string, actors.InvocationRequest) (actors.InvocationResponse, error) {
	s.calls++
	return s.response, s.err
}

func TestClientInvokesAddresslessDialIn(t *testing.T) {
	stub := &dialInStub{response: actors.InvocationResponse{Result: &actors.InvocationResult{Outcome: "completed"}}}
	req := actors.InvocationRequest{AttemptID: "01A", RunID: "01R", NodeRunID: "01N"}
	req.Node.ID = "node"
	got, err := actors.NewClient().Invoke(context.Background(), actors.Endpoint{DialIn: stub, DialInNamespace: "ns", DialInActorKey: "company/test"}, req)
	if err != nil || got.Result == nil || stub.calls != 1 {
		t.Fatalf("response=%+v calls=%d err=%v", got, stub.calls, err)
	}
}

func TestAddresslessDialInFailureDoesNotInventOutboundTarget(t *testing.T) {
	stub := &dialInStub{err: errors.New("mailbox unavailable")}
	req := actors.InvocationRequest{AttemptID: "01A", RunID: "01R", NodeRunID: "01N"}
	req.Node.ID = "node"
	_, err := actors.NewClient().Invoke(context.Background(), actors.Endpoint{DialIn: stub, DialInActorKey: "company/test"}, req)
	if err == nil || stub.calls != 1 {
		t.Fatalf("calls=%d err=%v", stub.calls, err)
	}
}

func TestParseDialInResponses(t *testing.T) {
	response, err := actors.ParseInvocationResponse(202, []byte(`{"invocation_id":"inv-1","supports_cancellation":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !response.Async || response.Accepted == nil || response.Accepted.InvocationID != "inv-1" || response.Requests != 1 {
		t.Fatalf("response = %+v", response)
	}
	result, err := actors.ParseInvocationResponse(200, []byte(`{"outcome":"completed","output":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Async || result.Result == nil || result.Result.Outcome != "completed" {
		t.Fatalf("result = %+v", result)
	}
}

// A 200 that parses as JSON but carries no outcome is still a rejection, but
// it is a rejection with no underlying cause. Reporting it through %w against
// a nil error would render the literal text "%!w(<nil>)" into the operator's
// message and leave the unwrap chain broken, so the two rejection reasons must
// be distinguishable: the malformed-JSON branch wraps, the missing-outcome
// branch does not. Asserting only err != nil would pass either way.
func TestParseDialInMissingOutcomeReportsCauseFreeError(t *testing.T) {
	_, err := actors.ParseInvocationResponse(200, []byte(`{}`))
	if err == nil {
		t.Fatal("a 200 without an outcome is not a result and must be rejected")
	}
	if strings.Contains(err.Error(), "%!w(") {
		t.Fatalf("missing-outcome error wraps a nil cause: %q", err.Error())
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("missing-outcome error has no cause to unwrap, got %v", unwrapped)
	}
}

func TestParseDialInMalformedBodyKeepsJSONCause(t *testing.T) {
	_, err := actors.ParseInvocationResponse(200, []byte(`{not json`))
	if err == nil {
		t.Fatal("a 200 that is not JSON is not a result and must be rejected")
	}
	if strings.Contains(err.Error(), "%!w(") {
		t.Fatalf("malformed-body error rendered a bad verb: %q", err.Error())
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("malformed-body error lost its json cause: %v (unwrap=%v)", err, errors.Unwrap(err))
	}
}

func TestParseDialInRefusalClassifiesLikeOutbound(t *testing.T) {
	_, err := actors.ParseInvocationResponse(403, []byte(`{"message":"no"}`))
	inv, ok := err.(*actors.InvocationError)
	if !ok || inv.Class != actors.ClassAuthOrPolicy {
		t.Fatalf("error = %#v", err)
	}
}
