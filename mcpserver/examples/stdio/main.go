// Command stdio serves three agenttool.New tools over MCP on stdin/stdout, so
// any MCP client can call Go tools written against the agenttool
// contract. It is also the server the interop test drives with the
// upstream MCP Inspector.
//
//	npx @modelcontextprotocol/inspector --cli go run ./examples/stdio --method tools/list
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/agenttool/mcpserver"
)

type upperArgs struct {
	Text string `json:"text" desc:"Text to uppercase"`
}

type addArgs struct {
	A int `json:"a" desc:"First addend"`
	B int `json:"b" desc:"Second addend"`
}

type sum struct {
	Sum int `json:"sum"`
}

// A 1x1 transparent PNG.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func main() {
	tools := agenttool.Set{
		agenttool.New("upper", "Uppercase a string", func(_ context.Context, a upperArgs) (string, error) {
			return strings.ToUpper(a.Text), nil
		}),
		agenttool.New("add", "Add two integers", func(_ context.Context, a addArgs) (sum, error) {
			return sum{Sum: a.A + a.B}, nil
		}, agenttool.WithStrict()),
		agenttool.New("tiny_image", "Return a 1x1 PNG", func(context.Context, agenttool.NoArgs) (openresponses.Contents, error) {
			return openresponses.Contents{&openresponses.InputImage{ImageURL: "data:image/png;base64," + tinyPNG}}, nil
		}),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv, err := mcpserver.NewServer("agenttool-stdio-example", "0.0.0", tools...)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
