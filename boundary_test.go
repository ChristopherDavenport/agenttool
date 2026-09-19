package agenttool

import (
	"go/build"
	"strings"
	"testing"
)

// TestImportBoundary enforces the rule that the contract imports
// openresponses and the standard library only, and never a loop, so
// typed tools are usable from any loop.
func TestImportBoundary(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		switch {
		case !strings.Contains(imp, "."):
			// standard library
		case imp == "github.com/ChristopherDavenport/openresponses":
		default:
			t.Errorf("agenttool imports %q; only openresponses and the standard library are allowed", imp)
		}
	}
}
