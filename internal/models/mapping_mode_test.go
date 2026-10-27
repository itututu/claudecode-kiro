package models

import "testing"

// TestEffectiveMappingMode pins the launcher's reuse decision to the same parse
// that actually serves traffic. A gateway reporting "builtin" while routing on
// overrides would let a launcher reuse a process that answers different models.
func TestEffectiveMappingMode(t *testing.T) {
	custom := `[{"anthropic":"my-alias","kiro":"claude-sonnet-4.6"}]`
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset", "", MappingModeBuiltin},
		{"explicitly empty array", `[]`, MappingModeBuiltin},
		// Malformed input is warned about and ignored, so the gateway really is
		// serving the built-in table: reporting "custom" here would be a lie.
		{"malformed json", `{not json`, MappingModeBuiltin},
		{"not an array", `{"anthropic":"x"}`, MappingModeBuiltin},
		{"one override", custom, MappingModeCustom},
		// Parsed but contentless, and parsed but identical to a built-in: both
		// still mean the caller supplied a table, so both are custom.
		{"empty object entry", `[{}]`, MappingModeCustom},
		{"duplicates a builtin", `[{"anthropic":"claude-opus-5","kiro":"claude-opus-5"}]`, MappingModeCustom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KIROCC_MODEL_MAPPINGS", tc.env)
			if got := EffectiveMappingMode(); got != tc.want {
				t.Errorf("EffectiveMappingMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEffectiveMappingModeAgreesWithResolution guards the invariant directly:
// the reported mode must match whether an override is actually in effect.
func TestEffectiveMappingModeAgreesWithResolution(t *testing.T) {
	t.Setenv("KIROCC_MODEL_MAPPINGS", `[{"anthropic":"my-alias","kiro":"claude-haiku-4.5"}]`)
	if got := EffectiveMappingMode(); got != MappingModeCustom {
		t.Fatalf("mode = %q, want custom", got)
	}
	kiro, _, _, _ := Resolve("my-alias", false)
	if kiro != "claude-haiku-4.5" {
		t.Errorf("override not in effect: Resolve(my-alias) kiro = %q", kiro)
	}
}
