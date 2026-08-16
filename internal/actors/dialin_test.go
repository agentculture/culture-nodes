package actors_test

import (
	"context"
	"errors"
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

func TestParseDialInRefusalClassifiesLikeOutbound(t *testing.T) {
	_, err := actors.ParseInvocationResponse(403, []byte(`{"message":"no"}`))
	inv, ok := err.(*actors.InvocationError)
	if !ok || inv.Class != actors.ClassAuthOrPolicy {
		t.Fatalf("error = %#v", err)
	}
}
