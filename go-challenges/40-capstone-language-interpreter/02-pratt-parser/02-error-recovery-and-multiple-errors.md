# Exercise 2: Error Recovery and the ErrParse Sentinel

A parser that stops at the first mistake is a poor tool: a programmer who left out three semicolons wants to hear about all three, not fix one and recompile to discover the next. This exercise builds the other half of a real parser — the part that collects every error it can in a single pass and keeps going. It is a deliberately small Pratt parser (just `let` statements and arithmetic) so the recovery machinery is the subject rather than a footnote: a `ParseError` type that carries a source location and wraps a package-level `ErrParse` sentinel through `Unwrap`, and a `skipToSync` routine that, after an error, advances to the next statement boundary so parsing can resume.

This module is fully self-contained. It begins with its own `go mod init`, defines its own lexer and AST, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
recovery.go         tiny lexer + AST + Pratt parser, ParseError, ErrParse, skipToSync
cmd/
  demo/
    main.go         parse a clean program, then a broken one and print every error
recovery_test.go    multi-error collection, ErrParse wrapping, resume-after-error
```

- Files: `recovery.go`, `cmd/demo/main.go`, `recovery_test.go`.
- Implement: `New(src string) *Parser`, `(*Parser).Parse() *Program`, `(*Parser).Errors()`, the `ParseError` type with `Error` and `Unwrap`, the `ErrParse` sentinel, and `skipToSync`.
- Test: `recovery_test.go` proves a clean program yields no errors, three broken statements yield at least three errors, every error satisfies `errors.Is(err, ErrParse)`, and a good statement after a bad one is still recovered.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p parserrecovery/cmd/demo && cd parserrecovery
go mod init example.com/parserrecovery
```

### Why recovery is a feature, not an afterthought

Stopping at the first syntax error is the easy thing to build and the wrong thing to ship. The information a compiler is most able to give cheaply — "here are the four places your syntax is broken" — is exactly the information a one-error-and-quit parser throws away. Recovery turns a single parse into a batch report, and it rests on two independent pieces that this module isolates so each can be seen clearly.

The first is a structured error. Returning a bare `fmt.Errorf` string loses the location and forces every caller to string-match if it wants to know whether the failure was a syntax problem or something else. A `ParseError` struct carries the line and column so a message can point at the offending token, and it implements `Unwrap() error` returning a shared `ErrParse` sentinel. That single method is what lets any caller write `errors.Is(err, ErrParse)` and get `true` for every parse error without the parser ever wrapping its errors by hand — the standard-library `errors.Is` walks the `Unwrap` chain and finds the sentinel. The struct stays a concrete, inspectable value (a caller can read `e.Line`); the sentinel gives it a category.

The second is resynchronization. After the parser detects an error mid-statement, the tokens immediately following it are untrustworthy — they are the tail of a construct the parser has already given up on. Continuing to parse them produces a cascade of derived, meaningless errors. `skipToSync` cuts that cascade: it discards tokens until it reaches a known-good boundary, here a semicolon or the start of the next `let`, so the next `parseStatement` begins on solid ground. The heuristic is not perfect — it can occasionally skip a real error or stop a token early — but in exchange it converts "one error then noise" into "one honest error per broken statement," which is what makes the multi-error report trustworthy.

### The parser with recovery built in

The grammar is intentionally minimal: `let name = expr;` statements and bare expression statements, where an expression is integers, identifiers, unary `-`, the four arithmetic operators with `*`/`/` binding tighter than `+`/`-`, and parentheses. That is enough to produce the three classic `let` errors — missing identifier, missing `=`, missing value — and to show recovery stepping over each one. The recovery contract runs through three methods. `expect` is the only place an "expected X, got Y" error is raised: it advances on a match and records a located `ParseError` on a mismatch. `skipToSync` is called by each statement parser the moment one of its `expect` checks fails, draining tokens up to the next boundary. And `parseExpression` reports "unexpected token" when the current token has no prefix handler, which is how a missing value (`let y = ;`) is caught. Every error funnels into the same `errors` slice, and because `ParseError.Unwrap` returns `ErrParse`, each one is simultaneously a precise, located value and a member of the `ErrParse` category.

Create `recovery.go`:

```go
// Package recovery is a small, self-contained Pratt parser whose purpose is to
// demonstrate error recovery: instead of stopping at the first syntax error it
// collects every error it can, resynchronizing at statement boundaries so that
// one pass over the source reports several mistakes. The grammar is deliberately
// tiny (let statements and arithmetic expressions) so the recovery machinery —
// the ParseError type, the ErrParse sentinel, and skipToSync — stands out
// without a full language around it.
package recovery

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// --- Tokens ------------------------------------------------------------------

// TokenType identifies a terminal symbol.
type TokenType int

const (
	EOF TokenType = iota
	ILLEGAL
	INT
	IDENT
	LET
	ASSIGN    // =
	PLUS      // +
	MINUS     // -
	STAR      // *
	SLASH     // /
	LPAREN    // (
	RPAREN    // )
	SEMICOLON // ;
)

var tokenNames = map[TokenType]string{
	EOF: "EOF", ILLEGAL: "ILLEGAL", INT: "INT", IDENT: "IDENT", LET: "let",
	ASSIGN: "=", PLUS: "+", MINUS: "-", STAR: "*", SLASH: "/",
	LPAREN: "(", RPAREN: ")", SEMICOLON: ";",
}

func (t TokenType) String() string {
	if s, ok := tokenNames[t]; ok {
		return s
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

// Token is one terminal symbol with its position.
type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Col     int
}

// Lexer scans a source string into tokens.
type Lexer struct {
	src  string
	pos  int
	line int
	col  int
}

// NewLexer returns a Lexer ready to scan src.
func NewLexer(src string) *Lexer {
	return &Lexer{src: src, line: 1, col: 1}
}

// Next returns the next token, advancing past it.
func (l *Lexer) Next() Token {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			l.advance()
			continue
		}
		break
	}
	if l.pos >= len(l.src) {
		return Token{Type: EOF, Line: l.line, Col: l.col}
	}

	line, col := l.line, l.col
	ch := l.src[l.pos]

	singles := map[byte]TokenType{
		'=': ASSIGN, '+': PLUS, '-': MINUS, '*': STAR, '/': SLASH,
		'(': LPAREN, ')': RPAREN, ';': SEMICOLON,
	}
	if tt, ok := singles[ch]; ok {
		l.advance()
		return Token{Type: tt, Literal: string(ch), Line: line, Col: col}
	}
	if ch >= '0' && ch <= '9' {
		start := l.pos
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.advance()
		}
		return Token{Type: INT, Literal: l.src[start:l.pos], Line: line, Col: col}
	}
	if ch == '_' || unicode.IsLetter(rune(ch)) {
		start := l.pos
		for l.pos < len(l.src) && (l.src[l.pos] == '_' ||
			unicode.IsLetter(rune(l.src[l.pos])) || (l.src[l.pos] >= '0' && l.src[l.pos] <= '9')) {
			l.advance()
		}
		lit := l.src[start:l.pos]
		if strings.ToLower(lit) == "let" {
			return Token{Type: LET, Literal: lit, Line: line, Col: col}
		}
		return Token{Type: IDENT, Literal: lit, Line: line, Col: col}
	}

	l.advance()
	return Token{Type: ILLEGAL, Literal: string(ch), Line: line, Col: col}
}

func (l *Lexer) advance() {
	if l.pos < len(l.src) {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

// --- AST ---------------------------------------------------------------------

// Node is the base interface for every AST node.
type Node interface{ String() string }

// IntegerLiteral is an integer constant.
type IntegerLiteral struct{ Value int64 }

func (i *IntegerLiteral) String() string { return strconv.FormatInt(i.Value, 10) }

// Identifier is a name.
type Identifier struct{ Name string }

func (id *Identifier) String() string { return id.Name }

// PrefixExpression is -x.
type PrefixExpression struct {
	Operator string
	Right    Node
}

func (pe *PrefixExpression) String() string { return "(" + pe.Operator + pe.Right.String() + ")" }

// InfixExpression is left op right.
type InfixExpression struct {
	Left     Node
	Operator string
	Right    Node
}

func (ie *InfixExpression) String() string {
	return "(" + ie.Left.String() + " " + ie.Operator + " " + ie.Right.String() + ")"
}

// LetStatement is let name = value.
type LetStatement struct {
	Name  string
	Value Node
}

func (ls *LetStatement) String() string {
	v := "<nil>"
	if ls.Value != nil {
		v = ls.Value.String()
	}
	return "let " + ls.Name + " = " + v
}

// ExpressionStatement is a bare expression used as a statement.
type ExpressionStatement struct{ Expr Node }

func (es *ExpressionStatement) String() string {
	if es.Expr == nil {
		return ""
	}
	return es.Expr.String()
}

// Program is the root of the AST.
type Program struct{ Statements []Node }

func (p *Program) String() string {
	parts := make([]string, len(p.Statements))
	for i, s := range p.Statements {
		parts[i] = s.String()
	}
	return strings.Join(parts, "\n")
}

// --- Errors ------------------------------------------------------------------

// ErrParse is the sentinel wrapped by every ParseError. Tests and callers that
// care only about "was this a parse error?" use errors.Is(err, ErrParse).
var ErrParse = errors.New("parse error")

// ParseError describes one syntax error with source location.
type ParseError struct {
	Line    int
	Col     int
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Message)
}

// Unwrap returns ErrParse so that errors.Is(err, ErrParse) is true for any
// *ParseError without the caller wrapping it manually.
func (e *ParseError) Unwrap() error { return ErrParse }

// --- Parser ------------------------------------------------------------------

// Precedence levels.
const (
	lowest  = iota
	sum     // + -
	product // * /
	prefix  // -x
)

var infixBP = map[TokenType]int{
	PLUS:  sum,
	MINUS: sum,
	STAR:  product,
	SLASH: product,
}

// Parser builds an AST and accumulates errors rather than aborting on the first.
type Parser struct {
	lex    *Lexer
	cur    Token
	peek   Token
	errors []*ParseError
}

// New returns a Parser ready to parse src.
func New(src string) *Parser {
	p := &Parser{lex: NewLexer(src)}
	p.advance()
	p.advance()
	return p
}

// Errors returns every error collected during Parse. Each wraps ErrParse.
func (p *Parser) Errors() []*ParseError { return p.errors }

func (p *Parser) advance() {
	p.cur = p.peek
	p.peek = p.lex.Next()
}

func (p *Parser) peekIs(tt TokenType) bool { return p.peek.Type == tt }

func (p *Parser) expect(tt TokenType) bool {
	if p.peekIs(tt) {
		p.advance()
		return true
	}
	p.errorf(p.peek.Line, p.peek.Col, "expected %s, got %s", tt, p.peek.Type)
	return false
}

func (p *Parser) errorf(line, col int, format string, args ...any) {
	p.errors = append(p.errors, &ParseError{Line: line, Col: col, Message: fmt.Sprintf(format, args...)})
}

// skipToSync advances past tokens until a statement boundary so that parsing can
// resume after an error. It stops on a semicolon (consumed by the caller's
// advance) or just before a token that begins a new statement.
func (p *Parser) skipToSync() {
	for p.cur.Type != EOF {
		if p.cur.Type == SEMICOLON {
			return
		}
		if p.peek.Type == LET {
			return
		}
		p.advance()
	}
}

// Parse parses the whole program, collecting errors as it goes.
func (p *Parser) Parse() *Program {
	prog := &Program{}
	for p.cur.Type != EOF {
		if s := p.parseStatement(); s != nil {
			prog.Statements = append(prog.Statements, s)
		}
		p.advance()
	}
	return prog
}

func (p *Parser) parseStatement() Node {
	if p.cur.Type == LET {
		return p.parseLetStatement()
	}
	return p.parseExpressionStatement()
}

func (p *Parser) parseLetStatement() Node {
	if !p.expect(IDENT) {
		p.skipToSync()
		return nil
	}
	name := p.cur.Literal
	if !p.expect(ASSIGN) {
		p.skipToSync()
		return nil
	}
	p.advance()
	val := p.parseExpression(lowest)
	if val == nil {
		p.skipToSync()
		return nil
	}
	if p.peekIs(SEMICOLON) {
		p.advance()
	}
	return &LetStatement{Name: name, Value: val}
}

func (p *Parser) parseExpressionStatement() Node {
	expr := p.parseExpression(lowest)
	if expr == nil {
		p.skipToSync()
		return nil
	}
	if p.peekIs(SEMICOLON) {
		p.advance()
	}
	return &ExpressionStatement{Expr: expr}
}

func (p *Parser) parseExpression(minBP int) Node {
	left := p.parsePrefix()
	if left == nil {
		return nil
	}
	for {
		bp, ok := infixBP[p.peek.Type]
		if !ok || bp <= minBP {
			break
		}
		op := p.peek.Literal
		p.advance()
		p.advance()
		right := p.parseExpression(bp)
		if right == nil {
			return nil
		}
		left = &InfixExpression{Left: left, Operator: op, Right: right}
	}
	return left
}

func (p *Parser) parsePrefix() Node {
	switch p.cur.Type {
	case INT:
		v, err := strconv.ParseInt(p.cur.Literal, 10, 64)
		if err != nil {
			p.errorf(p.cur.Line, p.cur.Col, "cannot parse %q as integer", p.cur.Literal)
			return nil
		}
		return &IntegerLiteral{Value: v}
	case IDENT:
		return &Identifier{Name: p.cur.Literal}
	case MINUS:
		op := p.cur.Literal
		p.advance()
		right := p.parseExpression(prefix)
		if right == nil {
			return nil
		}
		return &PrefixExpression{Operator: op, Right: right}
	case LPAREN:
		p.advance()
		expr := p.parseExpression(lowest)
		if expr == nil {
			return nil
		}
		if !p.expect(RPAREN) {
			return nil
		}
		return expr
	default:
		p.errorf(p.cur.Line, p.cur.Col, "unexpected token %s", p.cur.Type)
		return nil
	}
}
```

### The runnable demo

The demo runs both halves of the contract. It parses one clean program and reports zero errors with the parsed AST, then parses a program with three broken `let` statements and prints each collected error with its location, ending by classifying the first error through `errors.Is(err, ErrParse)`. The three errors come out one per line because `skipToSync` resynchronizes at each semicolon, so the report has exactly one honest entry per broken statement rather than a cascade.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/parserrecovery"
)

func main() {
	// A clean program parses with no errors.
	good := recovery.New("let total = 1 + 2 * 3;")
	prog := good.Parse()
	fmt.Println("ast:", prog.String())
	fmt.Println("errors:", len(good.Errors()))

	// A broken program collects every error in one pass instead of stopping.
	bad := recovery.New("let = 5;\nlet x 5;\nlet y = ;")
	bad.Parse()
	errs := bad.Errors()
	fmt.Println("collected", len(errs), "errors:")
	for _, e := range errs {
		fmt.Println("  ", e.Error())
	}
	fmt.Println("first is ErrParse:", errors.Is(errs[0], recovery.ErrParse))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ast: let total = (1 + (2 * 3))
errors: 0
collected 3 errors:
   1:5: expected IDENT, got =
   2:7: expected =, got INT
   3:9: unexpected token ;
first is ErrParse: true
```

### Tests

The tests pin the recovery contract from four angles. `TestValidProgramHasNoErrors` confirms the happy path leaves the error slice empty and produces the expected AST, so recovery never fires on clean input. `TestCollectsMultipleErrors` is the centerpiece: three broken statements must yield at least three errors in one pass, which only happens if `skipToSync` resumes after each. `TestParseErrorWrapsErrParse` checks that the first collected error satisfies `errors.Is(err, ErrParse)`, validating the `Unwrap` wiring. And `TestRecoveryContinuesAfterError` proves the parser does more than count failures — a well-formed `let` after a broken one is still parsed into the program. The `Example` pins the exact two-error count and the `errors.Is` classification as verified output.

Create `recovery_test.go`:

```go
package recovery

import (
	"errors"
	"fmt"
	"testing"
)

// TestValidProgramHasNoErrors confirms the happy path: a well-formed program
// parses with an empty error slice and the expected parenthesized AST.
func TestValidProgramHasNoErrors(t *testing.T) {
	t.Parallel()

	p := New("let total = 1 + 2 * 3;")
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got, want := prog.String(), "let total = (1 + (2 * 3))"; got != want {
		t.Fatalf("AST = %q, want %q", got, want)
	}
}

// TestCollectsMultipleErrors is the point of the module: three broken let
// statements must produce at least three errors in one pass, not stop at the
// first. skipToSync resynchronizes at each semicolon so each line is reported.
func TestCollectsMultipleErrors(t *testing.T) {
	t.Parallel()

	// Three distinct problems: missing ident, missing =, missing value.
	src := "let = 5;\nlet x 5;\nlet y = ;"
	p := New(src)
	p.Parse()
	errs := p.Errors()
	if len(errs) < 3 {
		t.Fatalf("expected at least 3 errors for %q, got %d: %v", src, len(errs), errs)
	}
}

// TestParseErrorWrapsErrParse checks that every collected error wraps the
// ErrParse sentinel, so a caller can classify it with errors.Is.
func TestParseErrorWrapsErrParse(t *testing.T) {
	t.Parallel()

	p := New("let = 5;")
	p.Parse()
	errs := p.Errors()
	if len(errs) == 0 {
		t.Fatal("expected at least one error")
	}
	if !errors.Is(errs[0], ErrParse) {
		t.Fatalf("errors.Is(errs[0], ErrParse) = false; error = %v", errs[0])
	}
}

// TestRecoveryContinuesAfterError verifies that a good statement following a
// bad one is still parsed, proving the parser resumed rather than aborted.
func TestRecoveryContinuesAfterError(t *testing.T) {
	t.Parallel()

	p := New("let x 5;\nlet y = 9;")
	prog := p.Parse()
	if len(p.Errors()) == 0 {
		t.Fatal("expected an error for the first statement")
	}
	if len(prog.Statements) != 1 {
		t.Fatalf("expected 1 recovered statement, got %d", len(prog.Statements))
	}
	if got, want := prog.Statements[0].String(), "let y = 9"; got != want {
		t.Fatalf("recovered statement = %q, want %q", got, want)
	}
}

// ExampleParser_errors prints the count of collected errors and shows that the
// first one is classified as a parse error via errors.Is.
func ExampleParser_errors() {
	p := New("let = 5;\nlet x 5;")
	p.Parse()
	errs := p.Errors()
	fmt.Println("errors:", len(errs))
	fmt.Println("is parse error:", errors.Is(errs[0], ErrParse))
	// Output:
	// errors: 2
	// is parse error: true
}
```

## Review

The recovery layer is correct when a broken source yields one honest error per broken statement and a clean source yields none. The two structural pieces carry that: `ParseError` keeps the location inspectable while `Unwrap` returning `ErrParse` makes every error answer `true` to `errors.Is(err, ErrParse)`, and `skipToSync` drains to the next boundary so the statement after a failure is parsed rather than buried under derived noise. Confirm that `expect` is the single origin of "expected X, got Y" errors, that each statement parser calls `skipToSync` immediately when an `expect` fails, and that a missing value is caught by `parseExpression` reporting "unexpected token" on a token with no prefix handler. The four tests plus the example passing under `go test -race ./...` establish those properties.

Two mistakes are easy to make here. The first is returning bare `fmt.Errorf` strings instead of a sentinel-wrapping type: callers then have to string-match to tell a syntax error from any other failure, and the location is gone. Implementing `Unwrap() error` on a concrete `ParseError` gives both a readable value and an `errors.Is` category for free. The second is omitting resynchronization — without `skipToSync`, a single error leaves the cursor inside the broken construct and the next `parseStatement` produces a chain of derived errors, so the report becomes one real problem followed by noise. Draining to a statement boundary is what keeps the multi-error report trustworthy.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and the `Unwrap` convention that the `ErrParse` sentinel relies on.
- [Writing An Interpreter In Go — Chapter 2: Parsing (Thorsten Ball)](https://interpreterbook.com/) — the Monkey parser and its `Errors()` collection that this module distills.
- [Simple but Powerful Pratt Parsing (matklad, 2020)](https://matklad.github.io/2020/04/13/simple-but-powerful-pratt-parsing.html) — the Pratt core the tiny grammar here is a cut-down version of.

---

Back to [01-binding-power-and-precedence.md](01-binding-power-and-precedence.md) | Next: [../03-ast-representation/00-concepts.md](../03-ast-representation/00-concepts.md)
