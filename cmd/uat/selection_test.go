package main

import (
	"context"
	"testing"
)

// allKindsFixture returns a deterministic mix of checks covering every
// CheckKind permutation we care about during selection. The order of
// the returned slice is the canonical declared order.
func allKindsFixture() []Check {
	noop := func(context.Context, *Env) error { return nil }
	return []Check{
		{Name: "n1", Kind: nonDestructive, Run: noop},
		{Name: "d1", Kind: destructive, Run: noop},
		{Name: "n2", Kind: nonDestructive, Run: noop},
		{Name: "r1", Kind: renderRequired, Run: noop},
		{Name: "nd1", Kind: nonDestructive | destructive, Run: noop},
		{Name: "nr1", Kind: nonDestructive | renderRequired, Run: noop},
		{Name: "dr1", Kind: destructive | renderRequired, Run: noop},
	}
}

func names(checks []Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectMode_FromConfig(t *testing.T) {
	if got := selectMode(config{}); got != modeExisting {
		t.Errorf("empty start-command should mean existing mode, got %v", got)
	}
	if got := selectMode(config{startCommand: "go run ."}); got != modeSelfManaged {
		t.Errorf("non-empty start-command should mean self-managed mode, got %v", got)
	}
}

func TestSelectChecks_ExistingModeExcludesDestructiveAndRenderRequired(t *testing.T) {
	cfg := config{}
	got := names(selectChecks(modeExisting, cfg, allKindsFixture()))
	want := []string{"n1", "n2"}
	if !equalStringSlices(got, want) {
		t.Errorf("existing mode selection = %v, want %v", got, want)
	}
}

func TestSelectChecks_ExistingModeWithRenderURLIncludesPureRenderRequired(t *testing.T) {
	cfg := config{renderURL: "http://render.example.com"}
	got := names(selectChecks(modeExisting, cfg, allKindsFixture()))
	// Pure render-required (r1) and non-destructive + render-required
	// (nr1) become safe when an explicit render URL is configured.
	// Destructive checks remain excluded.
	want := []string{"n1", "n2", "r1", "nr1"}
	if !equalStringSlices(got, want) {
		t.Errorf("existing mode w/ render-url selection = %v, want %v", got, want)
	}
}

func TestSelectChecks_SelfManagedModeIncludesAll(t *testing.T) {
	cfg := config{}
	got := names(selectChecks(modeSelfManaged, cfg, allKindsFixture()))
	want := []string{"n1", "d1", "n2", "r1", "nd1", "nr1", "dr1"}
	if !equalStringSlices(got, want) {
		t.Errorf("self-managed mode selection = %v, want %v", got, want)
	}
}

func TestSelectChecks_SkipDestructiveRemovesDestructiveInEitherMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode selectionMode
		cfg  config
		want []string
	}{
		{
			name: "existing+skip-destructive",
			mode: modeExisting,
			cfg:  config{skipDestructive: true},
			want: []string{"n1", "n2"},
		},
		{
			name: "self-managed+skip-destructive",
			mode: modeSelfManaged,
			cfg:  config{skipDestructive: true},
			want: []string{"n1", "n2", "r1", "nr1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := names(selectChecks(tt.mode, tt.cfg, allKindsFixture()))
			if !equalStringSlices(got, tt.want) {
				t.Errorf("selection = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectChecks_OrderIsStable(t *testing.T) {
	fixture := allKindsFixture()
	// Calling twice with the same inputs must yield the same order.
	a := names(selectChecks(modeSelfManaged, config{}, fixture))
	b := names(selectChecks(modeSelfManaged, config{}, fixture))
	if !equalStringSlices(a, b) {
		t.Errorf("selection not stable: %v vs %v", a, b)
	}
	// The selected order must be a subsequence of the declared order.
	declared := names(fixture)
	idx := 0
	for _, n := range a {
		for idx < len(declared) && declared[idx] != n {
			idx++
		}
		if idx == len(declared) {
			t.Fatalf("selected name %q out of declared order; selected=%v declared=%v", n, a, declared)
		}
		idx++
	}
}
