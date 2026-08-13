package actors_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// unmeasuredBlock is the exact shape the bridges' workspace.py emits when
// nothing could be measured: measured:false with every fact null. The merge
// tests below hold that this shape survives verbatim — never re-rendered as
// an empty diff, never dropped (issue #33a acceptance).
const unmeasuredBlock = `{"measured":false,"repo":null,"reason":"no repo configured","branch":null,` +
	`"head_before":null,"head_after":null,"status_porcelain":null,"changed_files":[],"diffstat":null}`

func mustJSON(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not valid JSON: %v\nraw: %s", err, raw)
	}
	return v
}

func TestMergeWorkspaceMeasuredFoldsBlockIntoOutputObject(t *testing.T) {
	measured := json.RawMessage(`{"measured":true,"repo":"/work/repo","reason":null,"branch":"main",` +
		`"head_before":"aaa","head_after":"bbb","status_porcelain":"","changed_files":["x.go"],"diffstat":" x.go | 2 +-"}`)
	merged := actors.MergeWorkspaceMeasured(json.RawMessage(`{"summary":"done","score":1}`), measured)

	var got struct {
		Summary           string          `json:"summary"`
		Score             float64         `json:"score"`
		WorkspaceMeasured json.RawMessage `json:"workspace_measured"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("merged output is not an object: %v\nmerged: %s", err, merged)
	}
	if got.Summary != "done" || got.Score != 1 {
		t.Errorf("the actor's own output fields were disturbed: %s", merged)
	}
	if !reflect.DeepEqual(mustJSON(t, got.WorkspaceMeasured), mustJSON(t, measured)) {
		t.Errorf("workspace_measured did not round-trip:\n sent: %s\n got: %s", measured, got.WorkspaceMeasured)
	}
}

// The unmeasured shape round-trips as-is: measured stays false, changed_files
// stays an empty list, diffstat stays an explicit null — the block is not
// normalised into something that reads like an empty diff, and it is not
// dropped.
func TestMergeWorkspaceMeasuredRoundTripsUnmeasuredVerbatim(t *testing.T) {
	merged := actors.MergeWorkspaceMeasured(json.RawMessage(`{"summary":"failed early"}`), json.RawMessage(unmeasuredBlock))

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("merged output is not an object: %v", err)
	}
	block, ok := got["workspace_measured"]
	if !ok {
		t.Fatalf("the unmeasured block was dropped: %s", merged)
	}
	if !reflect.DeepEqual(mustJSON(t, block), mustJSON(t, json.RawMessage(unmeasuredBlock))) {
		t.Errorf("the unmeasured block was altered:\n sent: %s\n got: %s", unmeasuredBlock, block)
	}
	// The facts the acceptance names, asserted directly rather than only by
	// deep equality: measured is false, and diffstat is a null, not "".
	var facts struct {
		Measured bool            `json:"measured"`
		Diffstat json.RawMessage `json:"diffstat"`
	}
	if err := json.Unmarshal(block, &facts); err != nil {
		t.Fatalf("block is not an object: %v", err)
	}
	if facts.Measured {
		t.Error("measured:false was rewritten to true")
	}
	if string(facts.Diffstat) != "null" {
		t.Errorf("diffstat = %s, want an explicit null (never an empty-diff rendering)", facts.Diffstat)
	}
}

func TestMergeWorkspaceMeasuredAbsentBlockLeavesOutputUntouched(t *testing.T) {
	output := json.RawMessage(`{"summary": "done"}`)
	for name, absent := range map[string]json.RawMessage{
		"nil":   nil,
		"empty": json.RawMessage(``),
		"null":  json.RawMessage(`null`),
	} {
		if got := actors.MergeWorkspaceMeasured(output, absent); string(got) != string(output) {
			t.Errorf("%s block changed the output byte-for-byte: %s", name, got)
		}
	}
}

func TestMergeWorkspaceMeasuredEmptyOutputBecomesBlockOnlyObject(t *testing.T) {
	for name, empty := range map[string]json.RawMessage{
		"nil":  nil,
		"null": json.RawMessage(`null`),
	} {
		merged := actors.MergeWorkspaceMeasured(empty, json.RawMessage(unmeasuredBlock))
		var got map[string]json.RawMessage
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("%s output did not merge into an object: %v", name, err)
		}
		if len(got) != 1 {
			t.Errorf("%s output merged into %d keys, want only workspace_measured: %s", name, len(got), merged)
		}
		if _, ok := got["workspace_measured"]; !ok {
			t.Errorf("%s output dropped the block: %s", name, merged)
		}
	}
}

// A non-object output cannot hold the key, and rewriting the actor's own
// answer to make room would corrupt what the node's contract validates.
func TestMergeWorkspaceMeasuredNonObjectOutputIsLeftAlone(t *testing.T) {
	for name, output := range map[string]json.RawMessage{
		"array":  json.RawMessage(`[1,2,3]`),
		"scalar": json.RawMessage(`"just a string"`),
	} {
		if got := actors.MergeWorkspaceMeasured(output, json.RawMessage(unmeasuredBlock)); string(got) != string(output) {
			t.Errorf("%s output was rewritten: %s", name, got)
		}
	}
}

// The typed top-level block is the bridge's measurement, structurally
// separate from the agent-authored output; on a key collision it wins.
func TestMergeWorkspaceMeasuredTopLevelBlockWinsOverInlineKey(t *testing.T) {
	merged := actors.MergeWorkspaceMeasured(
		json.RawMessage(`{"workspace_measured":{"measured":true,"forged":"by the agent"}}`),
		json.RawMessage(unmeasuredBlock),
	)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("merged output is not an object: %v", err)
	}
	if !reflect.DeepEqual(mustJSON(t, got["workspace_measured"]), mustJSON(t, json.RawMessage(unmeasuredBlock))) {
		t.Errorf("the agent's inline workspace_measured survived over the bridge-measured block: %s", merged)
	}
}

// Authority guard (issue #33a acceptance): workspace_measured is
// actor-reported data riding inside node output, and NOTHING on its landing
// path writes an evidence or observed-authority record from it. This is the
// grep, kept honest as a test: the only non-test Go files allowed to touch
// the field are the protocol declaration, the merge helper, and the two
// completion seams — none of which is a ledger writer. A new reference in an
// evidence-appending file (worker/hooks.go, runners/dispatch.go, the ledger
// or engine packages) fails here and must argue its authority first.
func TestWorkspaceMeasuredTouchesNoEvidenceWriters(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join("internal", "actors", "protocol.go"):  true,
		filepath.Join("internal", "actors", "workspace.go"): true,
		filepath.Join("internal", "actors", "callback.go"):  true,
		filepath.Join("internal", "worker", "dispatch.go"):  true,
	}

	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(source), "workspace_measured") &&
			!strings.Contains(string(source), "WorkspaceMeasured") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !allowed[rel] {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("workspace_measured is referenced outside its landing surface: %v\n"+
			"it is actor-reported node-output data (§10.4); if one of these files writes ledger records, "+
			"this reference risks laundering an actor claim into observed evidence", offenders)
	}
}
