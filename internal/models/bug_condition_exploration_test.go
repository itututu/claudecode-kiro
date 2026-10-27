package models

import "testing"

// TestProperty1BugConditionExplorationSupportedAliases checks the scoped
// canonical dotted, dashed, and dated aliases required by expectedBehavior.
//
// **Validates: Requirements 1.1–1.20, 2.1–2.20**
func TestProperty1BugConditionExplorationSupportedAliases(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{input: "claude-sonnet-4.6", want: "claude-sonnet-4.6"},
		{input: "claude-sonnet-4-6", want: "claude-sonnet-4.6"},
		{input: "claude-opus-4.6", want: "claude-opus-4.6"},
		{input: "claude-opus-4-6", want: "claude-opus-4.6"},
		{input: "claude-haiku-4.5", want: "claude-haiku-4.5"},
		{input: "claude-haiku-4-5", want: "claude-haiku-4.5"},
		{input: "claude-haiku-4-5-20251001", want: "claude-haiku-4.5"},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			got, _, _, _ := Resolve(testCase.input, false)
			if got != testCase.want {
				t.Errorf("counterexample: alias=%q effective_model=%q expected_model=%q", testCase.input, got, testCase.want)
			}
		})
	}
}
