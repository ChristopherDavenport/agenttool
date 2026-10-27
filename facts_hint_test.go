package agenttool

import (
	"context"
	"encoding/json"
	"testing"
)

// TestFactsOfAHint: facts marked as hints, from a source the host does
// not trust, keep their mark through FactsOf and Wrap, and lose their
// rewrite, which a reader ignores (#84).
func TestFactsOfAHint(t *testing.T) {
	tl := NewFunc("sh", "", nil, echo, WithFacts(func(context.Context, json.RawMessage) (Facts, error) {
		return Facts{Calls: []FactCall{{Tool: "read"}}, Rewrite: json.RawMessage(`{"stamp":"x"}`), Hint: true}, nil
	}))
	for name, t2 := range map[string]Tool{"tool": tl, "wrapped": Wrap(tl, tl.Execute)} {
		f, claims, err := FactsOf(context.Background(), t2, json.RawMessage(`{}`))
		if err != nil || !claims || !f.Hint || f.Rewrite != nil || len(f.Calls) != 1 {
			t.Errorf("%s: FactsOf = %+v, %v, %v; want a hint without its rewrite", name, f, claims, err)
		}
	}
}
