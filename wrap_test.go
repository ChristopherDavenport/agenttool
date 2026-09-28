package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

// everythingTool declares every property the contract has, so a wrapper
// that drops one is caught by comparing each reader against it.
type everythingTool struct {
	closed int
}

func (*everythingTool) Name() string                { return "all" }
func (*everythingTool) Description() string         { return "declares everything" }
func (*everythingTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*everythingTool) Execute(context.Context, Call) (Result, error) {
	return Text("inner"), nil
}
func (*everythingTool) Strict() bool     { return true }
func (*everythingTool) Sequential() bool { return false }
func (*everythingTool) Resource() string { return "shell:session" }
func (*everythingTool) Annotations() Annotations {
	return Annotations{Title: "All", ReadOnly: true, Idempotent: true}
}
func (*everythingTool) Confined(_ context.Context, args json.RawMessage) (bool, string) {
	return string(args) == `{"sandbox":true}`, "seatbelt"
}
func (*everythingTool) Replay(_ context.Context, args json.RawMessage) Replay {
	if string(args) == `{"sandbox":true}` {
		return ReplayKeyed
	}
	return ReplayUnknown
}
func (e *everythingTool) Close() error { e.closed++; return nil }

// readers is every way a harness asks a tool about itself. A wrapper is
// correct when each answers the same for it as for the tool it wraps.
func readers(ctx context.Context, t Tool) map[string]any {
	confined, by := ConfinedBy(ctx, t, json.RawMessage(`{"sandbox":true}`))
	_, closer := t.(io.Closer)
	return map[string]any{
		"closer":      closer,
		"name":        t.Name(),
		"description": t.Description(),
		"parameters":  string(t.Parameters()),
		"strict":      IsStrict(t),
		"sequential":  IsSequential(t),
		"resource":    ResourceOf(t),
		"annotations": AnnotationsOf(t),
		"confined":    confined,
		"confinedBy":  by,
		"replay":      ReplayOf(ctx, t, json.RawMessage(`{"sandbox":true}`)),
		"definition":  mustJSON(Definition(t)),
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestWrapForwardsEveryProperty(t *testing.T) {
	ctx := context.Background()
	inner := &everythingTool{}
	w := Wrap(inner, func(ctx context.Context, call Call) (Result, error) {
		return Text("outer"), nil
	})
	if got, want := readers(ctx, w), readers(ctx, inner); !reflect.DeepEqual(got, want) {
		t.Errorf("wrapper reads differently from the tool\n got: %v\nwant: %v", got, want)
	}
	res, err := w.Execute(ctx, Call{})
	if err != nil || res.Output.Text != "outer" {
		t.Errorf("Execute = %+v, %v; want the wrapper's function", res, err)
	}
	if err := (Set{w}).Close(); err != nil || inner.closed != 1 {
		t.Errorf("Close: err=%v closed=%d; want the inner tool closed once", err, inner.closed)
	}
	if Unwrap(w) != inner {
		t.Error("Unwrap should return the wrapped tool")
	}
	if Unwrap(inner) != nil {
		t.Error("Unwrap of a plain tool should be nil")
	}
}

// A wrapped tool that declares nothing reads as declaring nothing: the
// defaults, not a second set of them.
func TestWrapOfPlainToolReportsDefaults(t *testing.T) {
	ctx := context.Background()
	inner := NewFunc("plain", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil })
	w := Wrap(inner, inner.Execute)
	if got, want := readers(ctx, w), readers(ctx, inner); !reflect.DeepEqual(got, want) {
		t.Errorf("wrapper reads differently from the tool\n got: %v\nwant: %v", got, want)
	}
	if _, ok := w.(io.Closer); ok {
		t.Error("a wrapper around a tool that owns nothing should not be a closer")
	}
	if err := (Set{w}).Close(); err != nil {
		t.Errorf("Close of a set holding the wrapper: %v", err)
	}
}

// A sequential tool's resource reads as none through ResourceOf, on the
// wrapper as on the tool, and its raw Resource is still forwarded.
func TestWrapKeepsSequentialRule(t *testing.T) {
	inner := NewFunc("seq", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil },
		WithSequential(), WithResource("shell:session"))
	w := Wrap(inner, inner.Execute)
	if !IsSequential(w) || ResourceOf(w) != "" {
		t.Errorf("sequential=%v resource=%q; want true and none", IsSequential(w), ResourceOf(w))
	}
	if r := w.(Resource).Resource(); r != "shell:session" {
		t.Errorf("raw resource = %q, want the declared one", r)
	}
}

func TestWrapChainsAndCloseErrors(t *testing.T) {
	ctx := context.Background()
	inner := &everythingTool{}
	var order []string
	a := Wrap(inner, func(ctx context.Context, call Call) (Result, error) {
		order = append(order, "a")
		return inner.Execute(ctx, call)
	})
	b := Wrap(a, func(ctx context.Context, call Call) (Result, error) {
		order = append(order, "b")
		return a.Execute(ctx, call)
	})
	if _, err := b.Execute(ctx, Call{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, "") != "ba" {
		t.Errorf("order = %v", order)
	}
	if got, want := readers(ctx, b), readers(ctx, inner); !reflect.DeepEqual(got, want) {
		t.Errorf("two wrappers read differently from the tool\n got: %v\nwant: %v", got, want)
	}
	if Unwrap(Unwrap(b)) != inner {
		t.Error("Unwrap twice should reach the tool")
	}
}

func TestWrapPanicsOnNil(t *testing.T) {
	plain := NewFunc("p", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil })
	for name, fn := range map[string]func(){
		"nil tool": func() { Wrap(nil, plain.Execute) },
		"nil exec": func() { Wrap(plain, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			fn()
		})
	}
}

type closeFails struct{ Tool }

func (closeFails) Close() error { return errors.New("boom") }

func TestWrapForwardsCloseError(t *testing.T) {
	plain := NewFunc("p", "", nil, func(context.Context, Call) (Result, error) { return Result{}, nil })
	w := Wrap(closeFails{plain}, plain.Execute)
	if err := (Set{w}).Close(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Close = %v, want the inner error", err)
	}
}

// toolProperties reads the package's source for exported interface
// types and returns the methods of each that is a property of a tool:
// every one except those named here. A new optional interface therefore
// appears here the moment it is declared, and the tests below fail
// until every tool this package builds can carry it.
func toolProperties(t *testing.T) map[string][]string {
	t.Helper()
	notToolProperties := map[string]string{
		"Tool":       "the tool itself",
		"Recordable": "a property of a Details value",
		"Schemer":    "a property of an argument type",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	props := map[string][]string{}
	var found int
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				iface, isIface := ts.Type.(*ast.InterfaceType)
				if !isIface || !ts.Name.IsExported() {
					continue
				}
				found++
				if _, ok := notToolProperties[ts.Name.Name]; ok {
					continue
				}
				// Implementing the interface means having each method it
				// declares; every optional interface here declares its
				// own, so the names are enough.
				for _, m := range iface.Methods.List {
					for _, n := range m.Names {
						props[ts.Name.Name] = append(props[ts.Name.Name], n.Name)
					}
				}
			}
		}
	}
	if found < 8 {
		t.Fatalf("found %d exported interfaces, expected the package's", found)
	}
	return props
}

// TestWrapForwardsEveryOptionalInterface requires the wrapper to carry
// every optional interface the package declares, which is the promise
// Wrap's doc makes.
func TestWrapForwardsEveryOptionalInterface(t *testing.T) {
	wrapper := reflect.TypeFor[*wrapped]()
	for iface, methods := range toolProperties(t) {
		for _, m := range methods {
			if _, ok := wrapper.MethodByName(m); !ok {
				t.Errorf("optional interface %s is not forwarded by Wrap: wrapped has no %s; add it, or list %s as not a tool property", iface, m, iface)
			}
		}
	}
}

// TestNewCanDeclareEveryOptionalInterface requires the tools New and
// NewFunc build to carry every optional interface too, with an option
// to set it, so a tool built here never has to be embedded, which drops
// the rest, to gain one. io.Closer is not the package's and is checked
// by name.
func TestNewCanDeclareEveryOptionalInterface(t *testing.T) {
	props := toolProperties(t)
	props["io.Closer"] = []string{"Close"}
	for _, built := range []reflect.Type{
		reflect.TypeFor[*typedCloser[NoArgs, string]](),
		reflect.TypeFor[*funcToolCloser](),
	} {
		for iface, methods := range props {
			for _, m := range methods {
				if _, ok := built.MethodByName(m); !ok {
					t.Errorf("%s cannot declare %s: it has no %s; give it the method and an Option that sets it", built, iface, m)
				}
			}
		}
	}
}
