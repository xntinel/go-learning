# Exercise 2: Exposing resources and prompt templates to LLM clients

Tools are the effectful surface; they are not the whole protocol. This exercise
adds the other two MCP primitives: resources — read-only documents addressable by
URI, side-effect-free and cacheable — and prompts — parameterized message
templates a client can fetch and fill. The teaching point is the read-versus-effect
split: a static runbook is a resource, not a tool.

This module is fully self-contained, with its own `go mod init`, document store,
resource handler, prompt handler, demo, and tests. It imports no other exercise.

## What you'll build

```text
runbookmcp/                 independent module: example.com/runbookmcp
  go.mod                    go 1.26; requires the official MCP SDK
  runbook.go                Store of docs; resource handler (by URI); incident_summary prompt; NewServer wiring both
  cmd/
    demo/
      main.go               in-process client+server: list, read a resource, fetch a prompt
  runbook_test.go           in-memory client/server tests: list/read resources, unknown URI, render prompt, missing arg
```

- Files: `runbook.go`, `cmd/demo/main.go`, `runbook_test.go`.
- Implement: a `Store` of documents registered as resources (URI + MIME type), a single resource handler that dispatches by `req.Params.URI`, and a parameterized `incident_summary` prompt whose handler renders a message sequence from its arguments.
- Test: assert `ListResources` reports the registered URIs and MIME types, `ReadResource` returns the expected text and MIME type, an unknown URI errors, `GetPrompt` renders the expected message, and the prompt handler wraps a sentinel error for a missing required argument.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/runbookmcp/cmd/demo
cd ~/go-exercises/runbookmcp
go mod init example.com/runbookmcp
go mod edit -go=1.26
go get github.com/modelcontextprotocol/go-sdk@latest
```

### Resources: addressable, read-only, cacheable

A resource is identified by a URI (which must be absolute — it needs a scheme
such as `file://`, or `AddResource` panics) and carries a MIME type. The client
discovers resources with `resources/list` and reads one with `resources/read`.
The contract is that reads are safe and repeatable: the host can cache a resource
by URI and hand the bytes to the model without any effect on your system. That is
why a config document or a runbook belongs here and not behind a tool — modeling
it as a tool would deny the host caching and addressability and mislead the model
into treating a lookup as an action.

The resource handler has this shape:

```go
func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error)
```

You read the requested URI from `req.Params.URI`, look up the document, and return
a `*mcp.ReadResourceResult` whose `Contents` is a slice of `*mcp.ResourceContents`.
Each `ResourceContents` carries the `URI`, the `MIMEType`, and either `Text` (for
textual content) or `Blob` (base64 for binary). The SDK routes only URIs that match
a registered resource to your handler, so an unregistered URI is rejected before
your code runs; the handler still returns `mcp.ResourceNotFoundError(uri)` as a
defensive belt-and-suspenders for the miss. Note the exact field casing: it is
`MIMEType`, not `MimeType`.

### Prompts: parameterized templates, user-invoked

A prompt is a named, parameterized template the client fetches with `prompts/get`,
typically surfacing to the user as a reusable command rather than being auto-called
by the model. Its handler renders a sequence of messages from the supplied
arguments:

```go
func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error)
```

Arguments arrive as `req.Params.Arguments`, a `map[string]string`. The result's
`Messages` is a slice of `*mcp.PromptMessage`, each with a `Role` (a string type,
so `"user"` or `"assistant"`) and a `Content` — here a `*mcp.TextContent`. Unlike
a tool, a prompt handler that returns an error surfaces it as a protocol error, so
we validate the required argument and return a wrapped sentinel; the `GetPrompt`
call then fails cleanly.

Create `runbook.go`:

```go
package runbookmcp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrMissingArg is returned by a prompt handler when a required argument is absent.
var ErrMissingArg = errors.New("missing required prompt argument")

// Doc is a read-only reference document exposed as an MCP resource.
type Doc struct {
	URI  string
	Name string
	MIME string
	Body string
}

// Store holds the documents the server serves, keyed by URI.
type Store struct {
	docs map[string]Doc
}

// NewStore returns a store seeded with a runbook and a config document.
func NewStore() *Store {
	s := &Store{docs: make(map[string]Doc)}
	s.add(Doc{
		URI:  "file:///runbooks/disk-full.md",
		Name: "disk-full runbook",
		MIME: "text/markdown",
		Body: "# Disk full\n\n1. Identify the largest directories.\n2. Rotate and compress logs.\n3. Page SRE if usage stays above 90 percent.\n",
	})
	s.add(Doc{
		URI:  "file:///config/limits.json",
		Name: "service limits",
		MIME: "application/json",
		Body: `{"max_connections":1000,"request_timeout_seconds":30}`,
	})
	return s
}

func (s *Store) add(d Doc) { s.docs[d.URI] = d }

// read dispatches a resources/read by URI. The SDK only routes registered URIs
// here; the not-found path is defensive.
func (s *Store) read(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	d, ok := s.docs[uri]
	if !ok {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      d.URI,
			MIMEType: d.MIME,
			Text:     d.Body,
		}},
	}, nil
}

// incidentSummary renders a status-update prompt from its arguments. A missing
// required argument is a protocol error, so it returns a wrapped sentinel.
func incidentSummary(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	id := req.Params.Arguments["record_id"]
	if id == "" {
		return nil, fmt.Errorf("incident_summary: %w: record_id", ErrMissingArg)
	}
	tone := req.Params.Arguments["tone"]
	if tone == "" {
		tone = "concise"
	}
	return &mcp.GetPromptResult{
		Description: "Summarize an incident record for a status update.",
		Messages: []*mcp.PromptMessage{
			{
				Role:    "user",
				Content: &mcp.TextContent{Text: fmt.Sprintf("Write a %s status update for incident %s. Consult the runbook resource if relevant.", tone, id)},
			},
		},
	}, nil
}

// NewServer builds a server exposing every document as a resource and the
// incident_summary prompt. Registering them before Connect advertises the
// "resources" and "prompts" capabilities.
func NewServer(store *Store) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "runbooks", Version: "v1.0.0"}, nil)

	uris := make([]string, 0, len(store.docs))
	for uri := range store.docs {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		d := store.docs[uri]
		s.AddResource(&mcp.Resource{
			URI:         d.URI,
			Name:        d.Name,
			MIMEType:    d.MIME,
			Description: "Read-only reference document.",
		}, store.read)
	}

	s.AddPrompt(&mcp.Prompt{
		Name:        "incident_summary",
		Description: "Render a status-update prompt for an incident record.",
		Arguments: []*mcp.PromptArgument{
			{Name: "record_id", Description: "the incident record id", Required: true},
			{Name: "tone", Description: "writing tone; defaults to concise", Required: false},
		},
	}, incidentSummary)

	return s
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/runbookmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx := context.Background()

	server := runbookmcp.NewServer(runbookmcp.NewStore())
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		log.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "demo", Version: "v1.0.0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///config/limits.json"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s -> %s\n", res.Contents[0].MIMEType, res.Contents[0].Text)

	pr, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "incident_summary",
		Arguments: map[string]string{"record_id": "rec-001"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(pr.Messages[0].Content.(*mcp.TextContent).Text)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
application/json -> {"max_connections":1000,"request_timeout_seconds":30}
Write a concise status update for incident rec-001. Consult the runbook resource if relevant.
```

### Tests

As in Exercise 1, the tests wire an in-process client and server with
`mcp.NewInMemoryTransports`, so no network is involved. They assert the values the
handlers produced: the resource list carries the URIs and MIME types, a read
returns the exact stored text and MIME type, an unknown URI makes `ReadResource`
return an error, and the prompt renders the templated message. The missing-argument
case is asserted against the sentinel by calling the prompt handler directly.

Create `runbook_test.go`:

```go
package runbookmcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()

	server := NewServer(NewStore())
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestListResources(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	res, err := cs.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	got := make(map[string]string, len(res.Resources))
	for _, r := range res.Resources {
		got[r.URI] = r.MIMEType
	}
	want := map[string]string{
		"file:///runbooks/disk-full.md": "text/markdown",
		"file:///config/limits.json":    "application/json",
	}
	for uri, mime := range want {
		if got[uri] != mime {
			t.Errorf("resource %s MIME = %q, want %q", uri, got[uri], mime)
		}
	}
}

func TestReadResource(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	res, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "file:///runbooks/disk-full.md"})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	c := res.Contents[0]
	if c.MIMEType != "text/markdown" {
		t.Errorf("MIMEType = %q, want text/markdown", c.MIMEType)
	}
	if !strings.Contains(c.Text, "# Disk full") {
		t.Errorf("Text missing heading: %q", c.Text)
	}
}

func TestReadUnknownResource(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	_, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "file:///nope"})
	if err == nil {
		t.Fatal("ReadResource(unknown) returned nil error, want an error")
	}
}

func TestGetPrompt(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	pr, err := cs.GetPrompt(t.Context(), &mcp.GetPromptParams{
		Name:      "incident_summary",
		Arguments: map[string]string{"record_id": "rec-001"},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(pr.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(pr.Messages))
	}
	text := pr.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "rec-001") || !strings.Contains(text, "concise") {
		t.Errorf("rendered prompt = %q; want it to mention rec-001 and the default tone", text)
	}
}

func TestListPrompts(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	res, err := cs.ListPrompts(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	found := false
	for _, p := range res.Prompts {
		if p.Name == "incident_summary" {
			found = true
		}
	}
	if !found {
		t.Fatal("incident_summary not present in ListPrompts")
	}
}

func TestPromptMissingArg(t *testing.T) {
	t.Parallel()
	_, err := incidentSummary(context.Background(), &mcp.GetPromptRequest{
		Params: &mcp.GetPromptParams{Name: "incident_summary"},
	})
	if !errors.Is(err, ErrMissingArg) {
		t.Fatalf("err = %v, want wrapped ErrMissingArg", err)
	}
}

func Example() {
	ctx := context.Background()
	server := NewServer(NewStore())

	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		log.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ex", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///config/limits.json"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Contents[0].MIMEType)
	fmt.Println(res.Contents[0].Text)
	// Output:
	// application/json
	// {"max_connections":1000,"request_timeout_seconds":30}
}
```

## Review

The server is correct when a resource read is a pure function of the URI: the same
URI always returns the same bytes and MIME type, and an unregistered URI errors.
`TestReadResource` pins the text and MIME type; `TestReadUnknownResource` proves
the miss path errors rather than returning empty content. For prompts,
`TestGetPrompt` confirms the template renders the arguments (and the default tone),
and `TestPromptMissingArg` confirms the required argument is enforced with a
sentinel you can assert via `errors.Is`.

The mistake to avoid is blurring the primitives. A resource read must never have a
side effect — if you find yourself wanting to record, mutate, or charge for a read,
it is a tool, not a resource. Watch the field casing (`MIMEType`, and the pointer
slice `[]*mcp.ResourceContents`); getting either wrong is a compile error the gate
catches, but the more insidious version is returning content under a different URI
than requested, which breaks host caching. Keep the resource handler total over its
registered URIs and let the SDK reject the rest.

## Resources

- [`mcp` package reference](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp) — `AddResource`, `ResourceHandler`, `ReadResourceResult`, `ResourceContents`, `AddPrompt`, `PromptHandler`, `GetPromptResult`, `PromptMessage`.
- [MCP specification — Resources](https://modelcontextprotocol.io/specification) — URIs, MIME types, and the read-only, cacheable contract.
- [MCP specification — Prompts](https://modelcontextprotocol.io/specification) — parameterized templates and the `prompts/get` flow.

---

Back to [01-stdio-tools-server.md](01-stdio-tools-server.md) | Next: [03-streamable-http-with-auth.md](03-streamable-http-with-auth.md)
