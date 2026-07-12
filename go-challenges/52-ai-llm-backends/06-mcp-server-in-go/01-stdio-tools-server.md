# Exercise 1: A stdio MCP server with typed, schema-validated tools

The everyday MCP task is not a greeter. It is taking an internal service you
already own — a repository, a search index, a ticketing system — and exposing a
few of its operations to an LLM host without opening a hole. This exercise builds
that: an MCP server over the stdio transport with two typed tools, whose
arguments the model constructs and whose failures come back as recoverable tool
errors rather than crashes.

This module is fully self-contained. It has its own `go mod init`, defines the
repository and both tools inline, and ships its own demo and tests. Nothing here
imports another exercise.

## What you'll build

```text
recordsmcp/                 independent module: example.com/recordsmcp
  go.mod                    go 1.26; requires the official MCP SDK
  records.go                Repo (in-memory store); SearchInput/Output, CreateInput/Output; NewServer wiring two tools
  cmd/
    demo/
      main.go               in-process client+server: search, a tool error, an idempotent create
  records_test.go           in-memory client/server tests: valid vs invalid calls, schema listing, sentinel errors
```

- Files: `records.go`, `cmd/demo/main.go`, `records_test.go`.
- Implement: a `Repo` with `search_records` and `create_record` tools registered via the generic `mcp.AddTool`, input/output structs carrying `json` + `jsonschema` tags, handlers that validate arguments and return tool errors, and idempotent create.
- Test: `mcp.NewInMemoryTransports` wires an in-process client and server; assert valid calls return structured output, invalid calls return `IsError` results (not protocol errors), `ListTools` exposes the generated schema, and the handler wraps sentinel errors.
- Verify: `go test -count=1 -race ./...`

Set up the module. The SDK is an external dependency, so add it after `init`:

```bash
mkdir -p go-solutions/52-ai-llm-backends/06-mcp-server-in-go/01-stdio-tools-server/cmd/demo
cd go-solutions/52-ai-llm-backends/06-mcp-server-in-go/01-stdio-tools-server
go mod edit -go=1.26
go get github.com/modelcontextprotocol/go-sdk@latest
```

### The mental model: a typed RPC over reflected schema

An MCP tool is an RPC whose contract the model reads as JSON Schema. The generic
`mcp.AddTool[In, Out]` is the ergonomic surface a backend engineer wants: you
write a Go handler with a typed input struct and a typed output struct, and the
SDK infers the input schema from `In`, the output schema from `Out`, and validates
incoming arguments against the input schema before your handler runs. Field names
come from `json` tags; the per-field documentation the model sees comes from
`jsonschema` tags. Because that schema is the model's only spec for your tool,
the descriptions are load-bearing — a precise "between 1 and 100" on the limit is
the difference between a correct call and a guessed one.

The handler signature is fixed by the SDK:

```go
func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error)
```

Most handlers ignore `req` and the `*mcp.CallToolResult` return, returning
`(nil, out, nil)` on success: the SDK then fills the result's structured content
from `out` and mirrors it as JSON text content. Methods on your repository match
this signature directly, so `mcp.AddTool(s, tool, repo.search)` binds a method
value as the handler.

### Why a returned error is the right way to fail

The most important thing to internalize is what happens when a handler returns an
`error`. Under the generic `ToolHandlerFor`, a returned error is not a protocol
failure that aborts the RPC — the SDK packs the error text into the result's
`Content` and sets `IsError` to true, so the model receives it as a recoverable
tool error and can retry with better arguments. That is exactly what you want for
validation and business failures. So `return nil, CreateOutput{}, fmt.Errorf(...)`
on an empty title is correct: the model sees "title must not be empty" and fixes
its call. Reserve genuine protocol errors for the framework (unknown tool); do not
try to manufacture them for ordinary business conditions.

We still keep package-level sentinel errors wrapped with `%w` so the handler is
unit-testable with `errors.Is`, independent of the RPC round-trip. Across the wire
the error becomes text (the model cannot `errors.Is`), but inside the process the
sentinels give the tests a precise assertion.

### Validation and idempotency are yours to enforce

The schema handles shape; you handle semantics. `search_records` bounds the limit,
because a model can send `0` or `10000`. `create_record` rejects a blank title.
And because an agent retries, `create_record` accepts an optional idempotency key:
a repeated call with the same key returns the original record with `created:false`
instead of inserting a duplicate. That single field turns an at-least-once caller
into an exactly-once effect.

Create `records.go`:

```go
package recordsmcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Sentinel errors let the handlers be unit-tested with errors.Is. Across the RPC
// boundary they become tool-error text the model reads.
var (
	// ErrEmptyTitle is returned when create_record is called with a blank title.
	ErrEmptyTitle = errors.New("title must not be empty")
	// ErrBadLimit is returned when search_records is called with an out-of-range limit.
	ErrBadLimit = errors.New("limit out of range")
)

const maxSearchLimit = 100

// Record is one row of the internal repository exposed to the model.
type Record struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// Repo is a concurrency-safe in-memory store standing in for a real backend
// (a database, a search index, a ticketing system).
type Repo struct {
	mu      sync.Mutex
	records map[string]Record
	idem    map[string]string // idempotency key -> record ID
	seq     int
}

// NewRepo returns an empty repository.
func NewRepo() *Repo {
	return &Repo{records: make(map[string]Record), idem: make(map[string]string)}
}

// Seed inserts a record directly. It is a test/demo helper, not a tool.
func (r *Repo) Seed(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[rec.ID] = rec
}

// SearchInput is the argument surface of search_records. The jsonschema tags are
// the model's documentation for each field.
type SearchInput struct {
	Query string `json:"query" jsonschema:"case-insensitive substring matched against a record title; empty matches all records"`
	Limit int    `json:"limit" jsonschema:"maximum number of records to return, between 1 and 100"`
}

// SearchOutput is the structured result of search_records.
type SearchOutput struct {
	Records []Record `json:"records" jsonschema:"the matching records, ordered by id"`
	Count   int      `json:"count" jsonschema:"number of records returned"`
}

// search is a read query with parameters, so it is modeled as a tool rather than
// a resource. It returns results ordered by id for determinism.
func (r *Repo) search(_ context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	if in.Limit < 1 || in.Limit > maxSearchLimit {
		return nil, SearchOutput{}, fmt.Errorf("search_records: limit must be 1..%d, got %d: %w", maxSearchLimit, in.Limit, ErrBadLimit)
	}
	q := strings.ToLower(in.Query)

	r.mu.Lock()
	defer r.mu.Unlock()

	var matches []Record
	for _, rec := range r.records {
		if q == "" || strings.Contains(strings.ToLower(rec.Title), q) {
			matches = append(matches, rec)
		}
	}
	slices.SortFunc(matches, func(a, b Record) int { return cmp.Compare(a.ID, b.ID) })
	if len(matches) > in.Limit {
		matches = matches[:in.Limit]
	}
	return nil, SearchOutput{Records: matches, Count: len(matches)}, nil
}

// CreateInput is the argument surface of create_record.
type CreateInput struct {
	Title          string `json:"title" jsonschema:"human-readable title of the record; must be non-empty"`
	Owner          string `json:"owner" jsonschema:"the user or team that owns the record"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"optional client key; a repeated call with the same key returns the original record instead of creating a duplicate"`
}

// CreateOutput is the structured result of create_record.
type CreateOutput struct {
	ID      string `json:"id" jsonschema:"the id assigned to the record"`
	Created bool   `json:"created" jsonschema:"true if a new record was created, false if an existing one was returned for a repeated idempotency key"`
}

// create is the effectful tool. It validates its argument and is idempotent under
// a client-supplied key, because an agent may retry the same step.
func (r *Repo) create(_ context.Context, _ *mcp.CallToolRequest, in CreateInput) (*mcp.CallToolResult, CreateOutput, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, CreateOutput{}, fmt.Errorf("create_record: %w", ErrEmptyTitle)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if in.IdempotencyKey != "" {
		if id, ok := r.idem[in.IdempotencyKey]; ok {
			return nil, CreateOutput{ID: id, Created: false}, nil
		}
	}
	r.seq++
	id := fmt.Sprintf("rec-%03d", r.seq)
	r.records[id] = Record{ID: id, Title: in.Title, Owner: in.Owner, Status: "open"}
	if in.IdempotencyKey != "" {
		r.idem[in.IdempotencyKey] = id
	}
	return nil, CreateOutput{ID: id, Created: true}, nil
}

// NewServer builds an MCP server exposing the repository's tools. Registering the
// tools before Connect is what advertises the "tools" capability to clients.
func NewServer(repo *Repo) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "records", Version: "v1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_records",
		Description: "Search records by a case-insensitive substring of their title.",
	}, repo.search)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_record",
		Description: "Create a record. Pass idempotency_key to make retries safe.",
	}, repo.create)
	return s
}

// RunStdio serves the server over stdio, the transport a host uses when it runs
// this binary as a co-located subprocess.
func RunStdio(ctx context.Context, repo *Repo) error {
	return NewServer(repo).Run(ctx, &mcp.StdioTransport{})
}
```

In production `main` you would call `RunStdio(context.Background(), repo)` and the
host would speak newline-delimited JSON over the process's stdin and stdout. The
demo below instead wires an in-process client to the same server with
`NewInMemoryTransports`, so you can watch the round-trips without a host.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"example.com/recordsmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func decode(v, dst any) {
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, dst)
}

func main() {
	ctx := context.Background()

	repo := recordsmcp.NewRepo()
	repo.Seed(recordsmcp.Record{ID: "seed-1", Title: "disk full on api-1", Owner: "sre", Status: "open"})
	repo.Seed(recordsmcp.Record{ID: "seed-2", Title: "latency spike on checkout", Owner: "payments", Status: "open"})

	server := recordsmcp.NewServer(repo)
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

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_records",
		Arguments: map[string]any{"query": "disk", "limit": 10},
	})
	if err != nil {
		log.Fatal(err)
	}
	var so recordsmcp.SearchOutput
	decode(res.StructuredContent, &so)
	fmt.Printf("search found %d: %s\n", so.Count, so.Records[0].Title)

	// A bad argument becomes a tool error, not a transport failure.
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_record",
		Arguments: map[string]any{"title": "", "owner": "sre"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("create empty title: isError=%v\n", bad.IsError)

	// The same idempotency key twice: created once, deduplicated on retry.
	for i := range 2 {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "create_record",
			Arguments: map[string]any{"title": "rotate certs", "owner": "sre", "idempotency_key": "k-42"},
		})
		if err != nil {
			log.Fatal(err)
		}
		var co recordsmcp.CreateOutput
		decode(res.StructuredContent, &co)
		fmt.Printf("create #%d: id=%s created=%v\n", i+1, co.ID, co.Created)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
search found 1: disk full on api-1
create empty title: isError=true
create #1: id=rec-001 created=true
create #2: id=rec-001 created=false
```

### Tests

The tests need no network: `mcp.NewInMemoryTransports` returns a linked pair of
transports, one for the server's `Connect` and one for the client's, so requests
travel in-process. The table drives valid and invalid calls and asserts that a
bad argument yields a result with `IsError` set — a tool error the model can see —
rather than a Go error from `CallTool`, which would be a protocol failure. A
separate test confirms `ListTools` carries the reflected input schema, and a
direct-handler test asserts the sentinel error with `errors.Is`.

Create `records_test.go`:

```go
package recordsmcp

import (
	"context"
	"encoding/json"
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

	repo := NewRepo()
	repo.Seed(Record{ID: "seed-1", Title: "disk full on api-1", Owner: "sre", Status: "open"})
	repo.Seed(Record{ID: "seed-2", Title: "latency spike on checkout", Owner: "payments", Status: "open"})

	server := NewServer(repo)
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

func decode(t *testing.T, v, dst any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal into %T: %v", dst, err)
	}
}

func TestCallTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr bool
	}{
		{"search valid", "search_records", map[string]any{"query": "disk", "limit": 10}, false},
		{"search bad limit", "search_records", map[string]any{"query": "disk", "limit": 0}, true},
		{"create valid", "create_record", map[string]any{"title": "rotate certs", "owner": "sre"}, false},
		{"create empty title", "create_record", map[string]any{"title": "", "owner": "sre"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cs := newTestSession(t)
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
			if err != nil {
				t.Fatalf("CallTool returned a protocol error, want a tool result: %v", err)
			}
			if res.IsError != tt.wantErr {
				t.Fatalf("IsError = %v, want %v", res.IsError, tt.wantErr)
			}
		})
	}
}

func TestSearchStructuredOutput(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "search_records",
		Arguments: map[string]any{"query": "disk", "limit": 10},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	var out SearchOutput
	decode(t, res.StructuredContent, &out)
	if out.Count != 1 || out.Records[0].ID != "seed-1" {
		t.Fatalf("got count=%d records=%v; want one seed-1", out.Count, out.Records)
	}
}

func TestIdempotentCreate(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	call := func() CreateOutput {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "create_record",
			Arguments: map[string]any{"title": "rotate certs", "owner": "sre", "idempotency_key": "k-1"},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		var out CreateOutput
		decode(t, res.StructuredContent, &out)
		return out
	}
	first, second := call(), call()
	if !first.Created {
		t.Fatal("first create: Created = false, want true")
	}
	if second.Created {
		t.Fatal("second create with same key: Created = true, want false")
	}
	if first.ID != second.ID {
		t.Fatalf("ids differ under same idempotency key: %s vs %s", first.ID, second.ID)
	}
}

func TestListToolsSchema(t *testing.T) {
	t.Parallel()
	cs := newTestSession(t)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema []byte
	found := false
	for _, tool := range res.Tools {
		if tool.Name == "search_records" {
			found = true
			schema, err = json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal input schema: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("search_records not present in ListTools")
	}
	for _, want := range []string{"query", "limit"} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("input schema missing %q: %s", want, schema)
		}
	}
}

func TestCreateSentinelError(t *testing.T) {
	t.Parallel()
	repo := NewRepo()
	_, _, err := repo.create(context.Background(), nil, CreateInput{Title: "   ", Owner: "x"})
	if !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("err = %v, want wrapped ErrEmptyTitle", err)
	}
}

func Example() {
	ctx := context.Background()
	repo := NewRepo()
	server := NewServer(repo)

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

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_record",
		Arguments: map[string]any{"title": "rotate certs", "owner": "sre"},
	})
	if err != nil {
		log.Fatal(err)
	}
	var out CreateOutput
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	fmt.Printf("id=%s created=%v isError=%v\n", out.ID, out.Created, res.IsError)
	// Output: id=rec-001 created=true isError=false
}
```

## Review

The server is correct when a well-formed call returns structured output with
`IsError` false, and a bad argument returns a result with `IsError` true and a
`nil` Go error from `CallTool`. That split is the whole point: the `TestCallTool`
table would fail if a validation error escaped as a protocol error, because
`CallTool` would return non-nil and the test fatals on it. `TestSearchStructuredOutput`
confirms the typed `Out` survives the JSON round-trip; `TestListToolsSchema`
confirms the reflected schema actually reaches a client, which is what the model
reads.

The mistakes to avoid are the ones the concepts warn about. Do not model
`search_records` as anything other than a tool — it takes free-form arguments the
model reasons about, so it is a tool even though it does not mutate; the truly
static, addressable documents come back as resources in Exercise 2. Do not return
a bare Go error expecting it to "fail loudly"; under the generic handler it
becomes a recoverable tool error, which is what lets the agent self-correct. And
keep `create_record` idempotent — `TestIdempotentCreate` proves a repeated key
does not double-insert, which is the difference between a demo and a tool an agent
can safely retry. Run `go test -race` so the mutex guarding the maps is exercised
under concurrent calls.

## Resources

- [`mcp` package reference](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp) — `NewServer`, the generic `AddTool`, `ToolHandlerFor`, `CallToolResult`, and `NewInMemoryTransports`.
- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) — the official SDK README and runnable examples, including the stdio server shape.
- [MCP specification — Tools](https://modelcontextprotocol.io/specification) — how `tools/list` and `tools/call` work on the wire and the `isError` semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-resources-and-prompts.md](02-resources-and-prompts.md)
