// Package mcp serves the peer network to a model over the Model Context
// Protocol. Every tool is one of the use cases of the CLI (cli.Jobs), and the
// profile decides which tools exist at all.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Autarquico/codeflock/internal/buildinfo"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
)

// Profile is what a model may do. Write includes Read.
type Profile string

// The profiles of `cflk mcp serve`.
const (
	Read  Profile = "read"
	Write Profile = "write"
)

// ParseProfile reads the value of --profile.
func ParseProfile(s string) (Profile, error) {
	switch p := Profile(s); p {
	case Read, Write:
		return p, nil
	}
	return "", domain.NewError(domain.CodeInvalidRequest,
		fmt.Sprintf("unknown profile %q: use read or write", s), nil)
}

// Jobs is what the tools use of a peer: the use cases of the CLI, which
// cli.Jobs implements.
type Jobs interface {
	Run(ctx context.Context, req cli.RunRequest) (cli.JobView, error)
	List(ctx context.Context, peer string) (cli.JobList, error)
	Peers(ctx context.Context) ([]cli.PeerView, error)
	Show(ctx context.Context, id string) (cli.JobView, error)
	Stop(ctx context.Context, id string) (cli.JobView, error)
	Resume(ctx context.Context, id string) (cli.JobView, error)
	Screen(ctx context.Context, id string) (string, error)
	Send(ctx context.Context, id, text string) (cli.JobView, error)
	// Attach opens a window attached to the Job: the server has no terminal.
	Attach(ctx context.Context, id string, readOnly bool) error
}

var _ Jobs = cli.Jobs(nil)

// NewServer builds the MCP server of a profile. Only the tools the profile
// allows are registered, so asking for another is an unknown tool. diag
// receives the text of the errors the model is not shown.
func NewServer(profile Profile, jobs Jobs, diag io.Writer) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "codeflock", Version: buildinfo.Get().Version}, nil)
	t := tools{jobs: jobs, profile: profile, diag: diag}
	t.read(s)
	if profile == Write {
		t.write(s)
	}
	return s
}

// add registers a tool whose handler returns its result as any value. The
// output schema is declared by hand and kept small: the SDK would derive one
// from the Go type and fail on the first field it cannot describe. The input
// schema is derived from In without null: a model that sends null for an
// optional field is mistaken, and clients reject schemas with unions. A
// *domain.Error comes back as a tool error that keeps its code and details; a
// cancelled call propagates as a cancellation; any other error is CF_INTERNAL
// for the model, and its text goes to t.diag, since it can carry paths.
func add[In any](t tools, s *sdk.Server, name, description string, out *jsonschema.Schema, h func(context.Context, In) (any, error)) {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: input schema of %s: %v", name, err))
	}
	withoutNull(in)
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: out},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
			res, err := h(ctx, in)
			var coded *domain.Error
			if errors.As(err, &coded) {
				return toolError(coded), nil, nil
			}
			if err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return nil, nil, ctx.Err()
				}
				fmt.Fprintf(t.diag, "cflk mcp: %s: %v\n", name, err)
				return toolError(domain.NewError(domain.CodeInternal, "unexpected error; see the stderr of cflk mcp serve", nil)), nil, nil
			}
			return nil, res, nil
		})
}

// withoutNull removes "null" from the types of a schema and of everything
// inside it, so the field that Go derives as nullable (a slice, a pointer) is
// described by its plain type.
func withoutNull(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if s.Types != nil {
		s.Types = slices.DeleteFunc(s.Types, func(t string) bool { return t == "null" })
		if len(s.Types) == 1 {
			s.Type, s.Types = s.Types[0], nil
		}
	}
	for _, p := range s.Properties {
		withoutNull(p)
	}
	withoutNull(s.Items)
	withoutNull(s.AdditionalProperties)
}

func toolError(e *domain.Error) *sdk.CallToolResult {
	structured := map[string]any{"code": e.Code, "message": e.Message}
	if e.Details != nil {
		structured["details"] = e.Details
	}
	return &sdk.CallToolResult{
		IsError:           true,
		Content:           []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("%s: %s", e.Code, e.Message)}},
		StructuredContent: structured,
	}
}

// objectOf is the schema of an object with the named properties, all required.
func objectOf(props map[string]string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
	for name, typ := range props {
		s.Properties[name] = &jsonschema.Schema{Type: typ}
		s.Required = append(s.Required, name)
	}
	return s
}

// Serve runs s over in and out until in ends or ctx is done. out carries only
// JSON-RPC: whatever else there is to say goes to the diagnostics of the caller.
func Serve(ctx context.Context, s *sdk.Server, in io.Reader, out io.Writer) error {
	return s.Run(ctx, &sdk.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{out}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
