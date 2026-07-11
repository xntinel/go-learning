# 10. Go Language Server (LSP)

Building a Language Server Protocol server for Go combines three distinct problems: a binary-framed JSON-RPC transport, an in-memory document model with precise position arithmetic, and incremental source analysis using the Go type-checker from the standard library. The hard part is not any single piece but the interaction: the LSP position model uses 0-based line/character (UTF-16 code units), the `go/token` package uses 1-based line/column and byte offsets, and these two coordinate systems must convert cleanly at every request boundary. This lesson builds a working LSP server that supports hover, go-to-definition, diagnostics, and document synchronization using only the Go standard library.

```text
lsp/
  go.mod
  transport.go       JSON-RPC 2.0 framing over stdin/stdout
  protocol.go        LSP types
  store.go           document store and position conversion
  analysis.go        go/ast + go/types single-file analysis
  server.go          request dispatch and handlers
  lsp_test.go        table-driven tests for transport and store
  cmd/
    golsp/
      main.go        entry point
```

## Concepts

### The Language Server Protocol

LSP standardizes editor-to-server communication so one server works with VS Code, Neovim, Emacs, and any LSP-compatible editor. Messages are JSON-RPC 2.0 objects framed with an HTTP-like `Content-Length` header:

```
Content-Length: 97\r\n
\r\n
{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{...}}
```

Three message kinds:

- Request: has an `id` and a `method`; the server must send a response with the same `id`.
- Notification: has a `method` but no `id`; the server must not send a response.
- Response: has the same `id` as the request, plus either `result` or `error`.

The server starts by responding to the `initialize` request with a `ServerCapabilities` object that declares what features it supports. The client then sends an `initialized` notification. After that, the server receives `textDocument/*` requests and notifications as the user edits files.

### Document Synchronization

The server maintains an in-memory copy of every open file; it never reads from disk during a request. The `textDocument/didOpen` notification delivers the initial content. `textDocument/didChange` delivers changes, either as a full replacement (`range` is absent) or as a ranged edit. `textDocument/didClose` cleans up.

Every handler that produces diagnostics or analysis results reads from the in-memory store, not from the filesystem. This makes the server correct even when the on-disk file is ahead of or behind the editor.

### Go Source Analysis with the Standard Library

The Go standard library provides a complete compiler front-end:

- `go/token.FileSet` — tracks file names and line/column information for source positions; every parsed file must be registered in the same `FileSet`.
- `go/parser.ParseFile` — parses a single source file into an `*ast.File`; `parser.ParseComments` preserves doc comments for hover display.
- `go/types.Config.Check` — type-checks a slice of `*ast.File` objects and populates a `types.Info` struct with resolved identifiers.
- `types.Info.Uses` — maps each `*ast.Ident` that is a reference to the `types.Object` it refers to.
- `types.Info.Defs` — maps each `*ast.Ident` that is a declaration to the `types.Object` it declares.
- `go/importer.Default` — loads installed packages from `GOROOT`; used as the `Importer` in `types.Config` so that standard library imports resolve during type-checking.

For hover and go-to-definition, the server finds the `*ast.Ident` at the cursor position by walking the AST with `ast.Inspect`, then looks up the identifier in `Uses` or `Defs`.

### Position Model: Bytes, Lines, and UTF-16

`go/token` uses 1-based line and column numbers and byte offsets. LSP uses 0-based line and character numbers where "character" is a UTF-16 code unit count. For ASCII source code the two character models agree. For source containing non-ASCII characters (emoji, CJK, mathematical symbols), they diverge. A correct server converts LSP positions to byte offsets and back at every boundary. The conversion in this lesson handles UTF-8 correctly by iterating runes rather than bytes.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/lsp/cmd/golsp
cd ~/go-exercises/lsp
go mod init example.com/lsp
```

The package name is `lsp`. All non-main files use `package lsp`. Tests in the same package can access unexported fields.

### Exercise 1: JSON-RPC Transport and Protocol Types

Create `transport.go`:

```go
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Message is a JSON-RPC 2.0 message. ID is nil for notifications.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ReadMessage reads one LSP message from r.
// The format is "Content-Length: N\r\n\r\n" followed by N bytes of JSON.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	var contentLen int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("lsp: read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			s := strings.TrimPrefix(line, "Content-Length: ")
			contentLen, err = strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", s, err)
			}
		}
	}
	if contentLen <= 0 {
		return nil, fmt.Errorf("lsp: missing or zero Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("lsp: read body: %w", err)
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("lsp: unmarshal: %w", err)
	}
	return &msg, nil
}

// WriteMessage writes one LSP message to w.
func WriteMessage(w io.Writer, msg *Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("lsp: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("lsp: write body: %w", err)
	}
	return nil
}
```

Create `protocol.go`:

```go
package lsp

import "encoding/json"

// Position is a 0-based line and character (UTF-16 code unit) offset.
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range is a half-open interval [Start, End) in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location pairs a document URI with a Range.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier identifies a document by its URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// TextDocumentItem is a document with its content, delivered in didOpen.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// VersionedTextDocumentIdentifier identifies a specific version of a document.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentContentChangeEvent describes a change to a document.
// Range is nil for full-document replacement.
type TextDocumentContentChangeEvent struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

// InitializeParams is received in the "initialize" request.
type InitializeParams struct {
	RootURI string `json:"rootUri"`
}

// ServerCapabilities declares the server's supported features.
type ServerCapabilities struct {
	TextDocumentSync   int                `json:"textDocumentSync"` // 1=Full 2=Incremental
	HoverProvider      bool               `json:"hoverProvider"`
	DefinitionProvider bool               `json:"definitionProvider"`
	ReferencesProvider bool               `json:"referencesProvider"`
	CompletionProvider *CompletionOptions `json:"completionProvider,omitempty"`
}

// CompletionOptions configures the completion provider.
type CompletionOptions struct {
	TriggerCharacters []string `json:"triggerCharacters"`
}

// InitializeResult is sent in response to "initialize".
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// HoverResult is the response to "textDocument/hover".
type HoverResult struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent is a string with a kind ("plaintext" or "markdown").
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Diagnostic is an LSP diagnostic pushed via publishDiagnostics.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1=Error 2=Warning 3=Info 4=Hint
	Message  string `json:"message"`
	Source   string `json:"source"`
}

// PublishDiagnosticsParams is sent as a notification to the client.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// CompletionItem is one completion candidate.
type CompletionItem struct {
	Label      string `json:"label"`
	Kind       int    `json:"kind"` // 2=Method 3=Function 6=Variable 7=Class
	Detail     string `json:"detail,omitempty"`
	InsertText string `json:"insertText,omitempty"`
}

// TextEdit describes a single text replacement.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit maps document URIs to lists of edits (used for rename).
type WorkspaceEdit struct {
	Changes map[string][]TextEdit `json:"changes"`
}

// Notification and request parameter types.

// DidOpenTextDocumentParams is received on textDocument/didOpen.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeTextDocumentParams is received on textDocument/didChange.
type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent `json:"contentChanges"`
}

// DidCloseTextDocumentParams is received on textDocument/didClose.
type DidCloseTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// TextDocumentPositionParams is embedded in hover, definition, and reference requests.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// RenameParams is received on textDocument/rename.
type RenameParams struct {
	TextDocumentPositionParams
	NewName string `json:"newName"`
}

// RawParams is a lazily-decoded JSON value.
type RawParams = json.RawMessage
```

`ReadMessage` keeps reading headers until it finds a blank line, so it tolerates extra headers (such as `Content-Type`) that some editors send. `WriteMessage` serializes to JSON first, computes the byte length, and writes the framing header before the body.

### Exercise 2: Document Store and Position Conversion

Create `store.go`:

```go
package lsp

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// ErrDocumentNotOpen is returned when an operation targets a URI not in the store.
var ErrDocumentNotOpen = errors.New("lsp: document not open")

// DocumentStore holds the current content of every open text document.
// All methods are safe for concurrent use.
type DocumentStore struct {
	mu   sync.RWMutex
	docs map[string]*document
}

type document struct {
	uri     string
	version int
	text    string
}

// NewDocumentStore returns an empty document store.
func NewDocumentStore() *DocumentStore {
	return &DocumentStore{docs: make(map[string]*document)}
}

// Open records the initial content of a document delivered by didOpen.
func (s *DocumentStore) Open(uri string, version int, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[uri] = &document{uri: uri, version: version, text: text}
}

// Change replaces the full content of a document (TextDocumentSync=Full).
func (s *DocumentStore) Change(uri string, version int, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[uri] = &document{uri: uri, version: version, text: text}
}

// ApplyIncrementalChange applies a ranged edit to the stored document.
// The Range is expressed in LSP positions (0-based line/character).
func (s *DocumentStore) ApplyIncrementalChange(uri string, version int, r Range, newText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[uri]
	if !ok {
		return ErrDocumentNotOpen
	}
	start, err := PositionToOffset(doc.text, r.Start)
	if err != nil {
		return fmt.Errorf("lsp: range start: %w", err)
	}
	end, err := PositionToOffset(doc.text, r.End)
	if err != nil {
		return fmt.Errorf("lsp: range end: %w", err)
	}
	updated := doc.text[:start] + newText + doc.text[end:]
	s.docs[uri] = &document{uri: uri, version: version, text: updated}
	return nil
}

// Close removes a document from the store.
func (s *DocumentStore) Close(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, uri)
}

// Get returns the current text of a document or ErrDocumentNotOpen.
func (s *DocumentStore) Get(uri string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[uri]
	if !ok {
		return "", ErrDocumentNotOpen
	}
	return doc.text, nil
}

// PositionToOffset converts a 0-based LSP Position to a byte offset in text.
// The character field is treated as a count of UTF-8 runes (not UTF-16 code units)
// for simplicity; a production server would use the UTF-16 interpretation.
func PositionToOffset(text string, p Position) (int, error) {
	line := int(p.Line)
	char := int(p.Character)
	offset := 0
	for i := 0; i < line; i++ {
		idx := strings.IndexByte(text[offset:], '\n')
		if idx < 0 {
			return 0, fmt.Errorf("lsp: line %d out of range", line)
		}
		offset += idx + 1
	}
	remaining := text[offset:]
	runeOffset := 0
	for j := 0; j < char; j++ {
		if runeOffset >= len(remaining) {
			return 0, fmt.Errorf("lsp: character %d out of range on line %d", char, line)
		}
		_, size := utf8.DecodeRuneInString(remaining[runeOffset:])
		runeOffset += size
	}
	return offset + runeOffset, nil
}

// OffsetToPosition converts a byte offset in text to a 0-based LSP Position.
func OffsetToPosition(text string, offset int) Position {
	line, char := uint32(0), uint32(0)
	for i, r := range text {
		if i >= offset {
			break
		}
		if r == '\n' {
			line++
			char = 0
		} else {
			char++
		}
	}
	return Position{Line: line, Character: char}
}
```

`DocumentStore` uses a read-write mutex so concurrent reads (hover, definition) do not block each other, while writes (didChange) get exclusive access. `PositionToOffset` advances by rune so it correctly crosses multi-byte UTF-8 sequences.

### Exercise 3: Go Source Analysis

Create `analysis.go`:

```go
package lsp

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"strings"
)

// AnalysisResult holds the parsed and type-checked output for one Go source file.
type AnalysisResult struct {
	Fset *token.FileSet
	File *ast.File
	Info *types.Info
	Pkg  *types.Package
	Errs []error
}

// Analyze parses and type-checks a single Go source file.
// filename is used in error messages and as the package path.
// Type-checking errors are collected in AnalysisResult.Errs, not returned directly,
// so callers can still use partial results (Defs, Uses) even when errors exist.
func Analyze(filename, src string) *AnalysisResult {
	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, filename, src, parser.ParseComments)
	res := &AnalysisResult{Fset: fset, File: file}
	if parseErr != nil {
		res.Errs = append(res.Errs, parseErr)
		if file == nil {
			return res
		}
	}

	info := &types.Info{
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
		Types: make(map[ast.Expr]types.TypeAndValue),
	}
	conf := types.Config{
		Importer: importer.Default(),
		Error:    func(err error) { res.Errs = append(res.Errs, err) },
	}
	pkg, _ := conf.Check(filename, fset, []*ast.File{file}, info)
	res.Info = info
	res.Pkg = pkg
	return res
}

// HoverInfo returns a human-readable type description for the identifier
// whose declaration or use overlaps byte offset pos in the source.
func HoverInfo(res *AnalysisResult, pos int) (string, bool) {
	if res.File == nil || res.Info == nil {
		return "", false
	}
	tf := res.Fset.File(res.File.Pos())
	if tf == nil || pos < 0 || pos > tf.Size() {
		return "", false
	}
	tokenPos := tf.Pos(pos)

	var found string
	ast.Inspect(res.File, func(n ast.Node) bool {
		if n == nil || found != "" {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Pos() <= tokenPos && tokenPos <= id.End() {
			if obj, ok2 := res.Info.Uses[id]; ok2 {
				found = formatObject(obj, res.Pkg)
				return false
			}
			if obj, ok2 := res.Info.Defs[id]; ok2 {
				found = formatObject(obj, res.Pkg)
				return false
			}
		}
		return true
	})
	if found == "" {
		return "", false
	}
	return found, true
}

// formatObject returns the string representation of obj.
// Identifiers from currentPkg are printed without their package qualifier.
func formatObject(obj types.Object, currentPkg *types.Package) string {
	qf := func(p *types.Package) string {
		if currentPkg != nil && p == currentPkg {
			return ""
		}
		return p.Name()
	}
	return types.ObjectString(obj, qf)
}

// DefinitionLocation returns the declared position of the identifier at byte
// offset pos. It looks up the identifier in types.Info.Uses to find what it
// refers to, then maps the declaration's token.Pos back to a file/line/column.
func DefinitionLocation(res *AnalysisResult, pos int) (Location, bool) {
	if res.File == nil || res.Info == nil || res.Fset == nil {
		return Location{}, false
	}
	tf := res.Fset.File(res.File.Pos())
	if tf == nil || pos < 0 || pos > tf.Size() {
		return Location{}, false
	}
	tokenPos := tf.Pos(pos)

	var declPos token.Position
	found := false
	ast.Inspect(res.File, func(n ast.Node) bool {
		if found || n == nil {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if id.Pos() <= tokenPos && tokenPos <= id.End() {
			if obj, ok2 := res.Info.Uses[id]; ok2 {
				declPos = res.Fset.Position(obj.Pos())
				found = true
			}
		}
		return !found
	})
	if !found {
		return Location{}, false
	}
	// token.Position uses 1-based line/column; LSP uses 0-based.
	lspPos := Position{
		Line:      uint32(declPos.Line - 1),
		Character: uint32(declPos.Column - 1),
	}
	return Location{
		URI:   "file://" + declPos.Filename,
		Range: Range{Start: lspPos, End: lspPos},
	}, true
}

// collectDiagnostics analyzes src and converts all errors to LSP Diagnostics.
func collectDiagnostics(uri, src string) []Diagnostic {
	res := Analyze(uri, src)
	diags := make([]Diagnostic, 0, len(res.Errs))
	for _, err := range res.Errs {
		diags = append(diags, errToDiagnostic(err, src))
	}
	return diags
}

// errToDiagnostic converts a parse or type error to an LSP Diagnostic.
// scanner.ErrorList is produced by go/parser; individual errors from
// go/types arrive one at a time via the Config.Error callback.
func errToDiagnostic(err error, src string) Diagnostic {
	var pos Position
	var msg string
	switch e := err.(type) {
	case scanner.ErrorList:
		if len(e) > 0 {
			pos = OffsetToPosition(src, e[0].Pos.Offset)
			msg = e[0].Msg
		}
	default:
		msg = err.Error()
		// go/types prefixes errors with the filename and position; strip them.
		if i := strings.Index(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
	}
	return Diagnostic{
		Range:    Range{Start: pos, End: pos},
		Severity: 1,
		Message:  msg,
		Source:   "golsp",
	}
}
```

`Analyze` continues type-checking even after parse errors because `go/parser` often returns a partial AST. This lets the server show type errors on code that has a recoverable syntax error, which is the common editing-in-progress case.

### Exercise 4: Server, Demo, and Tests

Create `server.go`:

```go
package lsp

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
)

// Server dispatches LSP requests received over a JSON-RPC 2.0 channel.
type Server struct {
	r     *bufio.Reader
	w     io.Writer
	mu    sync.Mutex // guards w; only one goroutine writes at a time
	store *DocumentStore
	log   *slog.Logger
}

// NewServer returns a Server that reads from r and writes to w.
func NewServer(r io.Reader, w io.Writer, log *slog.Logger) *Server {
	return &Server{
		r:     bufio.NewReader(r),
		w:     w,
		store: NewDocumentStore(),
		log:   log,
	}
}

// Run reads LSP messages in a loop until r returns an error (typically io.EOF
// when the client closes the connection).
func (s *Server) Run() error {
	for {
		msg, err := ReadMessage(s.r)
		if err != nil {
			return err
		}
		s.dispatch(msg)
	}
}

func (s *Server) dispatch(msg *Message) {
	switch msg.Method {
	case "initialize":
		s.handleInitialize(msg)
	case "initialized":
		// notification; no response
	case "shutdown":
		s.respond(msg.ID, struct{}{}, nil)
	case "exit":
		// client is done
	case "textDocument/didOpen":
		s.handleDidOpen(msg)
	case "textDocument/didChange":
		s.handleDidChange(msg)
	case "textDocument/didClose":
		s.handleDidClose(msg)
	case "textDocument/hover":
		s.handleHover(msg)
	case "textDocument/definition":
		s.handleDefinition(msg)
	case "$/cancelRequest":
		// ignore; cancellation is best-effort
	default:
		if msg.ID != nil {
			s.respondError(msg.ID, -32601, "method not found: "+msg.Method)
		}
	}
}

func (s *Server) handleInitialize(msg *Message) {
	result := InitializeResult{
		Capabilities: ServerCapabilities{
			TextDocumentSync:   1, // Full replacement on every change
			HoverProvider:      true,
			DefinitionProvider: true,
			CompletionProvider: &CompletionOptions{
				TriggerCharacters: []string{"."},
			},
		},
	}
	result.ServerInfo.Name = "golsp"
	result.ServerInfo.Version = "0.1.0"
	s.respond(msg.ID, result, nil)
}

func (s *Server) handleDidOpen(msg *Message) {
	var p DidOpenTextDocumentParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.log.Error("didOpen", "err", err)
		return
	}
	s.store.Open(p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.Text)
	s.pushDiagnostics(p.TextDocument.URI, p.TextDocument.Text)
}

func (s *Server) handleDidChange(msg *Message) {
	var p DidChangeTextDocumentParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.log.Error("didChange", "err", err)
		return
	}
	for _, change := range p.ContentChanges {
		if change.Range == nil {
			s.store.Change(p.TextDocument.URI, p.TextDocument.Version, change.Text)
		} else {
			if err := s.store.ApplyIncrementalChange(
				p.TextDocument.URI, p.TextDocument.Version,
				*change.Range, change.Text,
			); err != nil {
				s.log.Error("incremental change", "err", err)
			}
		}
	}
	if text, err := s.store.Get(p.TextDocument.URI); err == nil {
		s.pushDiagnostics(p.TextDocument.URI, text)
	}
}

func (s *Server) handleDidClose(msg *Message) {
	var p DidCloseTextDocumentParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return
	}
	s.store.Close(p.TextDocument.URI)
	// Clear diagnostics for the closed file so the editor removes the squiggles.
	s.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{
		URI:         p.TextDocument.URI,
		Diagnostics: []Diagnostic{},
	})
}

func (s *Server) handleHover(msg *Message) {
	var p TextDocumentPositionParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.respondError(msg.ID, -32600, "invalid params")
		return
	}
	text, err := s.store.Get(p.TextDocument.URI)
	if err != nil {
		s.respond(msg.ID, nil, nil)
		return
	}
	res := Analyze(p.TextDocument.URI, text)
	offset, err := PositionToOffset(text, p.Position)
	if err != nil {
		s.respond(msg.ID, nil, nil)
		return
	}
	info, ok := HoverInfo(res, offset)
	if !ok {
		s.respond(msg.ID, nil, nil)
		return
	}
	s.respond(msg.ID, HoverResult{
		Contents: MarkupContent{Kind: "plaintext", Value: info},
	}, nil)
}

func (s *Server) handleDefinition(msg *Message) {
	var p TextDocumentPositionParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.respondError(msg.ID, -32600, "invalid params")
		return
	}
	text, err := s.store.Get(p.TextDocument.URI)
	if err != nil {
		s.respond(msg.ID, nil, nil)
		return
	}
	res := Analyze(p.TextDocument.URI, text)
	offset, err := PositionToOffset(text, p.Position)
	if err != nil {
		s.respond(msg.ID, nil, nil)
		return
	}
	loc, ok := DefinitionLocation(res, offset)
	if !ok {
		s.respond(msg.ID, nil, nil)
		return
	}
	s.respond(msg.ID, loc, nil)
}

func (s *Server) pushDiagnostics(uri, text string) {
	diags := collectDiagnostics(uri, text)
	s.notify("textDocument/publishDiagnostics", PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diags,
	})
}

func (s *Server) respond(id any, result any, rpcErr *RPCError) {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			s.log.Error("marshal result", "err", err)
			return
		}
		raw = b
	}
	s.write(&Message{JSONRPC: "2.0", ID: id, Result: raw, Error: rpcErr})
}

func (s *Server) respondError(id any, code int, message string) {
	s.respond(id, nil, &RPCError{Code: code, Message: message})
}

func (s *Server) notify(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		s.log.Error("marshal notification", "err", err)
		return
	}
	s.write(&Message{JSONRPC: "2.0", Method: method, Params: b})
}

func (s *Server) write(msg *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := WriteMessage(s.w, msg); err != nil {
		s.log.Error("write", "err", err)
	}
}
```

Create `cmd/golsp/main.go`:

```go
package main

import (
	"io"
	"log/slog"
	"os"

	"example.com/lsp"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := lsp.NewServer(os.Stdin, os.Stdout, log)
	if err := srv.Run(); err != nil && err != io.EOF {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
```

Create `lsp_test.go`:

```go
package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Transport tests

func TestReadWriteMessageRoundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *Message
	}{
		{
			name: "request with params",
			msg: &Message{
				JSONRPC: "2.0",
				ID:      float64(1),
				Method:  "textDocument/hover",
				Params:  json.RawMessage(`{"textDocument":{"uri":"file:///a.go"}}`),
			},
		},
		{
			name: "notification without id",
			msg: &Message{
				JSONRPC: "2.0",
				Method:  "initialized",
			},
		},
		{
			name: "response with result",
			msg: &Message{
				JSONRPC: "2.0",
				ID:      float64(2),
				Result:  json.RawMessage(`{"contents":{"kind":"plaintext","value":"func foo()"}}`),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := WriteMessage(&buf, tc.msg); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			got, err := ReadMessage(bufio.NewReader(&buf))
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if got.Method != tc.msg.Method {
				t.Errorf("Method = %q, want %q", got.Method, tc.msg.Method)
			}
		})
	}
}

func TestReadMessageRejectsEmptyContentLength(t *testing.T) {
	t.Parallel()

	// A message with no Content-Length header.
	raw := "X-Custom: value\r\n\r\n{}"
	_, err := ReadMessage(bufio.NewReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("expected error for missing Content-Length, got nil")
	}
}

// Document store tests

func TestDocumentStoreOpenGetClose(t *testing.T) {
	t.Parallel()

	store := NewDocumentStore()
	const uri = "file:///main.go"
	const text = "package main\n\nfunc main() {}\n"

	store.Open(uri, 1, text)

	got, err := store.Get(uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != text {
		t.Errorf("Get = %q, want %q", got, text)
	}

	store.Close(uri)
	_, err = store.Get(uri)
	if !errors.Is(err, ErrDocumentNotOpen) {
		t.Errorf("Get after Close: err = %v, want ErrDocumentNotOpen", err)
	}
}

func TestDocumentStoreFullChange(t *testing.T) {
	t.Parallel()

	store := NewDocumentStore()
	const uri = "file:///foo.go"
	store.Open(uri, 1, "package foo\n")
	store.Change(uri, 2, "package bar\n")

	got, _ := store.Get(uri)
	if got != "package bar\n" {
		t.Errorf("after Change, text = %q, want %q", got, "package bar\n")
	}
}

func TestDocumentStoreIncrementalChange(t *testing.T) {
	t.Parallel()

	store := NewDocumentStore()
	const uri = "file:///inc.go"
	store.Open(uri, 1, "hello\nworld\n")

	// Replace "world" on line 1 (characters 0-5) with "Go".
	err := store.ApplyIncrementalChange(uri, 2, Range{
		Start: Position{Line: 1, Character: 0},
		End:   Position{Line: 1, Character: 5},
	}, "Go")
	if err != nil {
		t.Fatalf("ApplyIncrementalChange: %v", err)
	}

	got, _ := store.Get(uri)
	const want = "hello\nGo\n"
	if got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestDocumentStoreIncrementalChangeNotOpen(t *testing.T) {
	t.Parallel()

	store := NewDocumentStore()
	err := store.ApplyIncrementalChange("file:///missing.go", 1, Range{}, "x")
	if !errors.Is(err, ErrDocumentNotOpen) {
		t.Errorf("err = %v, want ErrDocumentNotOpen", err)
	}
}

// Position conversion tests

func TestPositionToOffset(t *testing.T) {
	t.Parallel()

	const text = "hello\nworld\n"
	tests := []struct {
		pos  Position
		want int
	}{
		{Position{0, 0}, 0},  // 'h'
		{Position{0, 5}, 5},  // '\n' at end of first line
		{Position{1, 0}, 6},  // 'w'
		{Position{1, 5}, 11}, // '\n' at end of second line
	}
	for _, tc := range tests {
		got, err := PositionToOffset(text, tc.pos)
		if err != nil {
			t.Errorf("PositionToOffset(%v): %v", tc.pos, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PositionToOffset(%v) = %d, want %d", tc.pos, got, tc.want)
		}
	}
}

func TestPositionToOffsetOutOfRange(t *testing.T) {
	t.Parallel()

	_, err := PositionToOffset("hello\n", Position{Line: 99, Character: 0})
	if err == nil {
		t.Fatal("expected error for line out of range, got nil")
	}
}

func TestOffsetToPosition(t *testing.T) {
	t.Parallel()

	const text = "hello\nworld\n"
	tests := []struct {
		offset int
		want   Position
	}{
		{0, Position{0, 0}},
		{5, Position{0, 5}},
		{6, Position{1, 0}},
		{11, Position{1, 5}},
	}
	for _, tc := range tests {
		got := OffsetToPosition(text, tc.offset)
		if got != tc.want {
			t.Errorf("OffsetToPosition(%d) = %v, want %v", tc.offset, got, tc.want)
		}
	}
}

func ExampleOffsetToPosition() {
	const text = "hello\nworld\n"
	pos := OffsetToPosition(text, 7)
	fmt.Printf("line=%d char=%d\n", pos.Line, pos.Character)
	// Output: line=1 char=1
}

// Analysis test: uses go/parser and go/types from stdlib (no external deps).
// Source has no imports so importer.Default() is not exercised.

func TestHoverInfoForVarDeclaration(t *testing.T) {
	t.Parallel()

	const src = "package p\n\ntype Point struct{ X, Y int }\n\nvar origin Point\n"
	res := Analyze("p.go", src)
	if len(res.Errs) > 0 {
		t.Fatalf("Analyze errors: %v", res.Errs)
	}

	originIdx := strings.Index(src, "origin")
	info, ok := HoverInfo(res, originIdx)
	if !ok {
		t.Fatal("HoverInfo returned nothing for 'origin'")
	}
	if !strings.Contains(info, "origin") {
		t.Errorf("hover info %q does not contain 'origin'", info)
	}
}

func TestCollectDiagnosticsForSyntaxError(t *testing.T) {
	t.Parallel()

	// Missing closing brace causes a parse error.
	const src = "package p\n\nfunc bad() {\n"
	diags := collectDiagnostics("bad.go", src)
	if len(diags) == 0 {
		t.Fatal("expected at least one diagnostic for invalid source")
	}
	if diags[0].Severity != 1 {
		t.Errorf("severity = %d, want 1 (Error)", diags[0].Severity)
	}
}

// Your turn: add TestPositionRoundtrip that calls PositionToOffset then
// OffsetToPosition on several offsets in a multi-line string and asserts
// that the result equals the original position.
```

To run the server locally, configure VS Code by adding this to `.vscode/settings.json`:

```json
{
  "go.useLanguageServer": false,
  "[go]": {
    "editor.defaultFormatter": null
  }
}
```

Then start the server from the terminal:

```bash
go build -o golsp ./cmd/golsp
```

And test the transport layer manually:

```bash
printf 'Content-Length: 77\r\n\r\n{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"rootUri":""}}' | ./golsp
```

The server responds with a JSON-framed `InitializeResult` and then exits when stdin closes.

## Common Mistakes

### Forgetting That ReadMessage Buffers Data

Wrong: passing a plain `io.Reader` to `ReadMessage` and then trying to read more data from the same reader after `ReadMessage` returns.

What happens: `bufio.Reader` reads ahead; the remaining body bytes are consumed by the buffer. If you bypass the `bufio.Reader` and read from the underlying `io.Reader` directly, you get garbage.

Fix: pass the same `*bufio.Reader` to every call to `ReadMessage`. Never mix a `bufio.Reader` with direct reads from its underlying reader. The server in this lesson passes `s.r` (a `bufio.Reader`) to every `ReadMessage` call.

### Confusing token.Position Line/Column With LSP Line/Character

Wrong: using `declPos.Line` and `declPos.Column` from `go/token` directly as LSP `line` and `character`.

What happens: `go/token` is 1-based; LSP is 0-based. A declaration on line 1 in the editor becomes line 0 in LSP. The editor's cursor jumps to the wrong location.

Fix: subtract 1 from both before constructing the LSP `Position`, as in `DefinitionLocation`:
```go
lspPos := Position{
	Line:      uint32(declPos.Line - 1),
	Character: uint32(declPos.Column - 1),
}
```

### Sending a Response to a Notification

Wrong: returning a JSON response for a notification message (one without an `id` field).

What happens: many LSP clients treat an unexpected response as a protocol error and disconnect or log confusing warnings.

Fix: check `msg.ID != nil` before sending any response. Notifications (`initialized`, `didOpen`, `didChange`, `didClose`) must never generate a response. Only requests (those with an `id`) get a response. The `dispatch` function in this lesson guards `respondError` with `if msg.ID != nil`.

### Re-analyzing the File on Every Keystroke Without Debouncing

Wrong: calling `Analyze` synchronously inside `handleDidChange` for every change event.

What happens: for large files, type-checking blocks the request loop, delaying subsequent hover or definition requests. If the editor sends changes on every keystroke, the server spends all its time in `go/types.Config.Check`.

Fix for a production server: maintain a per-file analysis cache, invalidate it on change, and run `Analyze` in a background goroutine with a short debounce (100-200 ms). This lesson omits the cache to keep the structure clear but notes its necessity.

### Treating LSP Positions as Byte Offsets for Non-ASCII Source

Wrong: computing `offset = line * lineWidth + character` or indexing directly into the UTF-8 byte slice by character index.

What happens: for source containing multi-byte UTF-8 characters (such as string literals with non-ASCII content or identifiers with Unicode letters), the byte offset diverges from the character count. Hover targets the wrong token.

Fix: use `PositionToOffset` which advances rune by rune using `utf8.DecodeRuneInString`, ensuring that each character step crosses exactly one Unicode code point regardless of its byte width.

## Verification

From `~/go-exercises/lsp`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must complete without output (for the first two) and without failures (for the last two). The test suite validates the transport round-trip, the document store lifecycle, position conversion at boundary offsets, and diagnostics for syntactically invalid source.

To run the demo:

```bash
go build -o golsp ./cmd/golsp
printf 'Content-Length: 77\r\n\r\n{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"rootUri":""}}' | ./golsp
```

Expect a `Content-Length: ...` framed JSON response containing `"serverInfo":{"name":"golsp","version":"0.1.0"}`.

## Summary

- LSP messages are JSON-RPC 2.0 objects framed with `Content-Length: N\r\n\r\n`; `ReadMessage` and `WriteMessage` encapsulate the framing so handlers never see raw bytes.
- The document store is the single source of truth; it is updated by `didOpen`/`didChange`/`didClose` notifications and read by every analysis request.
- `go/parser.ParseFile` + `go/types.Config.Check` provide parsing and type-checking from the Go standard library; `types.Info.Uses` and `types.Info.Defs` map `*ast.Ident` nodes to resolved objects.
- Position conversion between LSP (0-based line/character) and `go/token` (1-based line/column, byte offset) is a correctness boundary that must be exercised by tests.
- `ast.Inspect` walks the AST to find the identifier at a cursor position; once the identifier is found, `types.Info.Uses` resolves it to its declaration for hover text and definition navigation.
- A production server adds a per-file analysis cache, a debounce on diagnostics, and an incremental importer; this lesson omits those to keep each layer independently understandable.

## What's Next

Next: [Dead Code Elimination Tool](../11-dead-code-elimination-tool/11-dead-code-elimination-tool.md).

## Resources

- [Language Server Protocol specification 3.17](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/) — authoritative definition of all message types, capability flags, and lifecycle events.
- [pkg.go.dev/go/types](https://pkg.go.dev/go/types) — `Config`, `Info`, `Object`, `ObjectString`, and `TypeString`; the primary reference for the type-checker API used in this lesson.
- [pkg.go.dev/go/ast](https://pkg.go.dev/go/ast) — `Inspect`, `File`, `Ident`, and all node types walked in `HoverInfo` and `DefinitionLocation`.
- [pkg.go.dev/go/parser](https://pkg.go.dev/go/parser) — `ParseFile` and parse mode flags; the entry point for converting source text to an AST.
- [gopls source (golang.org/x/tools/gopls)](https://github.com/golang/tools/tree/master/gopls) — the official Go language server; its `protocol/` package defines the full LSP type set and its `cache/` package shows the production analysis cache design.
