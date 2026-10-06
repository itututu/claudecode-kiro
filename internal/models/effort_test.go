package models

import "testing"

func TestResolveEffort(t *testing.T) {
	tests := []struct {
		name      string
		kiroModel string
		requested string
		want      string
	}{
		// opus-5.5 / 5 / 4.8 / 4.7 / sonnet-5.5 / sonnet-5: full enum including xhigh.
		{"opus-5.5 low", "claude-opus-5.5", "low", "low"},
		{"opus-5.5 medium", "claude-opus-5.5", "medium", "medium"},
		{"opus-5.5 high", "claude-opus-5.5", "high", "high"},
		{"opus-5.5 xhigh", "claude-opus-5.5", "xhigh", "xhigh"},
		{"opus-5.5 max", "claude-opus-5.5", "max", "max"},
		{"opus-5.5 invalid dropped", "claude-opus-5.5", "enabled", ""},
		{"opus-5 xhigh", "claude-opus-5", "xhigh", "xhigh"},
		{"opus-5 max", "claude-opus-5", "max", "max"},
		{"opus-5 low", "claude-opus-5", "low", "low"},
		{"opus-4.8 xhigh", "claude-opus-4.8", "xhigh", "xhigh"},
		{"opus-4.8 max", "claude-opus-4.8", "max", "max"},
		{"opus-4.8 low", "claude-opus-4.8", "low", "low"},
		{"opus-4.7 xhigh", "claude-opus-4.7", "xhigh", "xhigh"},
		{"sonnet-5.5 low", "claude-sonnet-5.5", "low", "low"},
		{"sonnet-5.5 high", "claude-sonnet-5.5", "high", "high"},
		{"sonnet-5.5 xhigh", "claude-sonnet-5.5", "xhigh", "xhigh"},
		{"sonnet-5.5 max", "claude-sonnet-5.5", "max", "max"},
		{"sonnet-5.5 invalid dropped", "claude-sonnet-5.5", "enabled", ""},
		{"sonnet-5 xhigh", "claude-sonnet-5", "xhigh", "xhigh"},
		{"sonnet-5 max", "claude-sonnet-5", "max", "max"},
		{"sonnet-5 low", "claude-sonnet-5", "low", "low"},

		// opus-4.6 / sonnet-4.6 family: no xhigh. xhigh downgrades to max.
		{"opus-4.6 high", "claude-opus-4.6", "high", "high"},
		{"opus-4.6 max", "claude-opus-4.6", "max", "max"},
		{"opus-4.6 xhigh downgrades to max", "claude-opus-4.6", "xhigh", "max"},
		{"sonnet-4.6 xhigh downgrades to max", "claude-sonnet-4.6", "xhigh", "max"},

		// Unsupported models: effort dropped entirely.
		{"opus-4.5 unsupported", "claude-opus-4.5", "max", ""},
		{"sonnet-4.5 unsupported", "claude-sonnet-4.5", "high", ""},
		{"haiku-4.5 unsupported", "claude-haiku-4.5", "xhigh", ""},
		{"sonnet-4 unsupported", "claude-sonnet-4", "high", ""},
		// Legacy -1m SKUs reject additionalModelRequestFields upstream.
		{"sonnet-4.6-1m unsupported", "claude-sonnet-4.6-1m", "high", ""},
		{"opus-4.6-1m unsupported", "claude-opus-4.6-1m", "medium", ""},
		{"unknown model unsupported", "some-other-model", "max", ""},

		// Unrecognized effort values are dropped, NOT silently promoted to max.
		{"opus-4.8 invalid value dropped", "claude-opus-4.8", "enabled", ""},
		{"opus-4.8 typo dropped", "claude-opus-4.8", "xhgih", ""},
		{"opus-4.6 invalid value dropped", "claude-opus-4.6", "ultra", ""},
		{"opus-4.6 typo not promoted", "claude-opus-4.6", "maxx", ""},

		// Empty requested effort: nothing sent regardless of model.
		{"opus-4.8 empty", "claude-opus-4.8", "", ""},
		{"unsupported empty", "claude-opus-4.5", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveEffort(tt.kiroModel, tt.requested); got != tt.want {
				t.Errorf("ResolveEffort(%q, %q) = %q, want %q", tt.kiroModel, tt.requested, got, tt.want)
			}
		})
	}
}
