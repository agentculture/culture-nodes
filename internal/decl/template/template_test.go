package template

import "testing"

func TestParseReferences(t *testing.T) {
	tests := []struct {
		input string
		want  []Reference
	}{
		{"{owner}", []Reference{{Step: "0", Name: "owner", DefaultPresent: false}}},
		{"{0:owner:x}", []Reference{{Step: "0", Name: "owner", Default: "x", DefaultPresent: true}}},
		{"{1:owner}", []Reference{{Step: "1", Name: "owner", DefaultPresent: false}}},
		{"{jira-intake:owner:}", []Reference{{Step: "jira-intake", Name: "owner", Default: "", DefaultPresent: true}}},
		{"{10:x:}", []Reference{{Step: "10", Name: "x", Default: "", DefaultPresent: true}}},
		{"{{", nil},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := Parse(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.References()) != len(tt.want) {
				t.Fatalf("references = %#v, want %#v", got.References(), tt.want)
			}
			for i, ref := range got.References() {
				if ref != tt.want[i] {
					t.Errorf("reference %d = %#v, want %#v", i, ref, tt.want[i])
				}
			}
		})
	}
}

func TestRenderFallbackRules(t *testing.T) {
	for _, tt := range []struct {
		input, want string
		values      map[string]string
	}{
		{"{owner}", "Ada", map[string]string{"0.owner": "Ada"}},
		{"{1:owner:unknown}", "unknown", nil},
		{"{jira-intake:owner:}", "", nil},
		{"{1:owner}", "{1:owner}", nil},
		{"{{", "{", nil},
		{"{10:x:}", "", nil},
	} {
		t.Run(tt.input, func(t *testing.T) {
			tpl, err := Parse(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			got := tpl.Render(func(ref Reference) (string, bool) { v, ok := tt.values[ref.Step+"."+ref.Name]; return v, ok })
			if got != tt.want {
				t.Errorf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRejectsMalformedReferences(t *testing.T) {
	for _, input := range []string{"{:owner}", "{1:}", "{owner", "{}"} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", input)
		}
	}
}
