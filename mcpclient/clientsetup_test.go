package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// startParams and startResult are a host's own method, of the kind dax
// sends to its executor to start a process there.
type startParams struct {
	sdk.ParamsBase
	Command string `json:"command"`
}

type startResult struct {
	sdk.ResultBase
	ID string `json:"id"`
}

const startMethod = "acme/process.start"

// newCustomServer is newServer that also answers startMethod.
func newCustomServer(t testing.TB) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	err := sdk.AddReceivingCustomMethod(server, startMethod,
		func(_ context.Context, _ *sdk.ServerSession, p *startParams) (*startResult, error) {
			return &startResult{ID: "proc-" + p.Command}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestClientSetupRegistersACustomMethod(t *testing.T) {
	var order []string
	s := connect(t, newCustomServer(t),
		WithClientSetup(func(c *sdk.Client) error {
			order = append(order, "first")
			return sdk.AddSendingCustomMethod[*startParams, *startResult](c, startMethod)
		}),
		WithClientSetup(nil),
		WithClientSetup(func(*sdk.Client) error {
			order = append(order, "second")
			return nil
		}))
	if want := []string{"first", "second"}; !slices.Equal(order, want) {
		t.Errorf("setups ran %v, want %v", order, want)
	}
	res, err := sdk.CallCustomMethod[*startParams, *startResult](context.Background(), s.Session(), startMethod, &startParams{Command: "sleep"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "proc-sleep" {
		t.Errorf("ID = %q, want proc-sleep", res.ID)
	}
}

func TestWithoutClientSetupNothingIsRegistered(t *testing.T) {
	s := connect(t, newCustomServer(t))
	_, err := sdk.CallCustomMethod[*startParams, *startResult](context.Background(), s.Session(), startMethod, &startParams{Command: "sleep"})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("CallCustomMethod without registration = %v, want not registered", err)
	}
}

// countingTransport counts the client's attempts to connect over it.
type countingTransport struct {
	sdk.Transport
	connects atomic.Int32
}

func (c *countingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	c.connects.Add(1)
	return c.Transport.Connect(ctx)
}

func TestClientSetupErrorFailsConnect(t *testing.T) {
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := newCustomServer(t).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	later := false
	tr := &countingTransport{Transport: ct}
	s, err := Connect(ctx, tr,
		WithClientSetup(func(*sdk.Client) error { return refused }),
		WithClientSetup(func(*sdk.Client) error { later = true; return nil }))
	if s != nil {
		_ = s.Close()
		t.Error("Connect returned a remote although setup failed")
	}
	if !errors.Is(err, refused) {
		t.Errorf("Connect = %v, want it to wrap the setup's error", err)
	}
	if later {
		t.Error("a setup after the failing one ran")
	}
	if n := tr.connects.Load(); n != 0 {
		t.Errorf("transport connected %d times, want none", n)
	}
}

// A host that tunnels a process over its executor's connection registers
// its own methods on the client and sends them on the session.
func ExampleWithClientSetup() {
	ctx := context.Background()
	server := sdk.NewServer(&sdk.Implementation{Name: "executor", Version: "1"}, nil)
	_ = sdk.AddReceivingCustomMethod(server, startMethod,
		func(_ context.Context, _ *sdk.ServerSession, p *startParams) (*startResult, error) {
			return &startResult{ID: "proc-" + p.Command}, nil
		})
	ct, st := sdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		panic(err)
	}

	remote, err := Connect(ctx, ct, WithClientSetup(func(c *sdk.Client) error {
		return sdk.AddSendingCustomMethod[*startParams, *startResult](c, startMethod)
	}))
	if err != nil {
		panic(err)
	}
	defer remote.Close()

	res, err := sdk.CallCustomMethod[*startParams, *startResult](ctx, remote.Session(), startMethod, &startParams{Command: "gopls"})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.ID)
	// Output: proc-gopls
}
