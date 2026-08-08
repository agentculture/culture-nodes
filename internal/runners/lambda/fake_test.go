package lambda_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeLambda is an in-process double for the two Lambda operations the
// adapter calls, speaking the same REST-JSON protocol aws-sdk-go-v2 sends:
//
//	GET  /2015-03-31/functions/{FunctionName}
//	POST /2015-03-31/functions/{FunctionName}/invocations
//
// It never checks SigV4 signatures — the adapter's test credentials are
// syntactically valid and never verified.
//
// The hit counters are the point of several tests rather than a convenience:
// "dispatch to an unregistered identity is refused" is only meaningful if
// nothing was sent, and the only way to prove nothing was sent is to count.
type fakeLambda struct {
	mu sync.Mutex

	// getCalls and invokeCalls count requests that reached the handler, by
	// operation, regardless of the response.
	getCalls    int
	invokeCalls int
	// invokedNames records the FunctionName path segment of every invoke, so
	// a test can prove *which* function was addressed.
	invokedNames []string
	// lastPayload is the request body of the most recent invoke.
	lastPayload []byte

	// functions maps a function name or ARN to its GetFunction response.
	functions map[string]functionDescription
	// invoke is the response the next invoke returns.
	invoke invokeResponse

	server *httptest.Server
}

// functionDescription is the deployed configuration the fake reports.
type functionDescription struct {
	ARN              string
	Version          string
	ResolvedImageURI string
	PackageType      string
	TimeoutSeconds   int
	MemoryMiB        int
	EphemeralMiB     int
	SubnetIDs        []string
	// httpStatus and errorType, when set, make GetFunction fail instead.
	httpStatus int
	errorType  string
}

// invokeResponse is what the fake's next invoke returns. Exactly one of the
// three shapes applies: an ordinary response, a handler error, or an API
// error.
type invokeResponse struct {
	// Payload is the function's returned document.
	Payload string
	// FunctionError is "Unhandled" or "Handled" when the handler raised.
	FunctionError string
	// ExecutedVersion is echoed in the response header.
	ExecutedVersion string
	// LogTail is the plain-text execution log; the fake base64-encodes it
	// into X-Amz-Log-Result when the request asked for LogType=Tail.
	LogTail string
	// RequestID is the x-amzn-RequestId header value. Empty means the
	// response carries no request id at all, which the adapter must handle
	// without inventing one.
	RequestID string
	// httpStatus and errorType, when set, make the invoke fail at the API
	// level instead (throttles, access denied, service faults).
	httpStatus int
	errorType  string
	errorMsg   string
}

func newFakeLambda(t *testing.T) *fakeLambda {
	t.Helper()
	f := &fakeLambda{functions: map[string]functionDescription{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// describe registers a function's GetFunction response.
func (f *fakeLambda) describe(arn string, d functionDescription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.ARN = arn
	if d.PackageType == "" {
		d.PackageType = "Image"
	}
	if d.Version == "" {
		d.Version = "7"
	}
	f.functions[arn] = d
}

// setInvoke installs the response the next invoke returns.
func (f *fakeLambda) setInvoke(r invokeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ExecutedVersion == "" {
		r.ExecutedVersion = "7"
	}
	if r.RequestID == "" && r.httpStatus == 0 {
		r.RequestID = defaultRequestID
	}
	f.invoke = r
}

const defaultRequestID = "8f5a4d2c-1111-4000-8000-0123456789ab"

// clearRequestID makes the next invoke response carry no request id at all,
// which is the case the adapter must handle without inventing one.
func (f *fakeLambda) clearRequestID() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invoke.RequestID = ""
}

// counts returns the two hit counters.
func (f *fakeLambda) counts() (get, invoke int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.invokeCalls
}

func (f *fakeLambda) payload(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	raw := append([]byte(nil), f.lastPayload...)
	f.mu.Unlock()

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode captured payload: %v\n%s", err, raw)
	}
	return out
}

func (f *fakeLambda) handle(w http.ResponseWriter, r *http.Request) {
	name, invocation, ok := parseFunctionPath(r.URL.EscapedPath())
	if !ok {
		writeAWSError(w, http.StatusNotFound, "ResourceNotFoundException", "fake lambda: unrouted path "+r.URL.Path)
		return
	}

	switch {
	case invocation && r.Method == http.MethodPost:
		f.handleInvoke(w, r, name)
	case !invocation && r.Method == http.MethodGet:
		f.handleGetFunction(w, name)
	default:
		writeAWSError(w, http.StatusBadRequest, "InvalidRequestContentException",
			"fake lambda: unsupported "+r.Method+" "+r.URL.Path)
	}
}

// parseFunctionPath pulls the (URL-escaped) function name out of the two
// paths this fake serves. ARNs contain colons, which the SDK escapes, so the
// name is unescaped here rather than matched literally.
func parseFunctionPath(escapedPath string) (name string, invocation bool, ok bool) {
	const prefix = "/2015-03-31/functions/"
	if !strings.HasPrefix(escapedPath, prefix) {
		return "", false, false
	}
	rest := strings.TrimPrefix(escapedPath, prefix)
	if trimmed, cut := strings.CutSuffix(rest, "/invocations"); cut {
		rest, invocation = trimmed, true
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return "", false, false
	}
	if decoded == "" {
		return "", false, false
	}
	return decoded, invocation, true
}

func (f *fakeLambda) handleGetFunction(w http.ResponseWriter, name string) {
	f.mu.Lock()
	f.getCalls++
	description, known := f.functions[name]
	f.mu.Unlock()

	if !known {
		writeAWSError(w, http.StatusNotFound, "ResourceNotFoundException",
			"fake lambda: function "+name+" does not exist")
		return
	}
	if description.httpStatus != 0 {
		writeAWSError(w, description.httpStatus, description.errorType, "fake lambda: injected GetFunction failure")
		return
	}

	body := map[string]any{
		"Configuration": map[string]any{
			"FunctionArn": description.ARN,
			"Version":     description.Version,
			"PackageType": description.PackageType,
			"Timeout":     description.TimeoutSeconds,
			"MemorySize":  description.MemoryMiB,
			"State":       "Active",
		},
		"Code": map[string]any{
			"ResolvedImageUri": description.ResolvedImageURI,
			"RepositoryType":   "ECR",
		},
	}
	if description.EphemeralMiB > 0 {
		body["Configuration"].(map[string]any)["EphemeralStorage"] = map[string]any{"Size": description.EphemeralMiB}
	}
	if len(description.SubnetIDs) > 0 {
		body["Configuration"].(map[string]any)["VpcConfig"] = map[string]any{"SubnetIds": description.SubnetIDs}
	}
	writeJSON(w, http.StatusOK, body)
}

func (f *fakeLambda) handleInvoke(w http.ResponseWriter, r *http.Request, name string) {
	payload, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.invokeCalls++
	f.invokedNames = append(f.invokedNames, name)
	f.lastPayload = payload
	response := f.invoke
	f.mu.Unlock()

	if response.httpStatus != 0 {
		writeAWSErrorWithRequestID(w, response.httpStatus, response.errorType, response.errorMsg, response.RequestID)
		return
	}

	if response.RequestID != "" {
		w.Header().Set("x-amzn-RequestId", response.RequestID)
	}
	if response.ExecutedVersion != "" {
		w.Header().Set("X-Amz-Executed-Version", response.ExecutedVersion)
	}
	if response.FunctionError != "" {
		w.Header().Set("X-Amz-Function-Error", response.FunctionError)
	}
	if response.LogTail != "" && strings.EqualFold(r.Header.Get("X-Amz-Log-Type"), "Tail") {
		w.Header().Set("X-Amz-Log-Result", base64.StdEncoding.EncodeToString([]byte(response.LogTail)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, response.Payload)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-RequestId", defaultRequestID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAWSError(w http.ResponseWriter, status int, errorType, message string) {
	writeAWSErrorWithRequestID(w, status, errorType, message, defaultRequestID)
}

func writeAWSErrorWithRequestID(w http.ResponseWriter, status int, errorType, message, requestID string) {
	if errorType == "" {
		errorType = "ServiceException"
	}
	if message == "" {
		message = "fake lambda: injected " + errorType
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Errortype", errorType)
	if requestID != "" {
		w.Header().Set("x-amzn-RequestId", requestID)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": errorType, "message": message})
}

// reportLine renders a Lambda REPORT line the way the platform emits it:
// tab-separated fields on a line following START/END.
func reportLine(requestID string, durationMs, billedMs, memorySizeMB, maxMemoryMB float64) string {
	return fmt.Sprintf("START RequestId: %s Version: 7\n"+
		"some function output\n"+
		"END RequestId: %s\n"+
		"REPORT RequestId: %s\tDuration: %.2f ms\tBilled Duration: %.0f ms\t"+
		"Memory Size: %.0f MB\tMax Memory Used: %.0f MB\tInit Duration: 312.44 ms\n",
		requestID, requestID, requestID, durationMs, billedMs, memorySizeMB, maxMemoryMB)
}
