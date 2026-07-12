# Exercise 4: Recursive-Descent Parser for an API Filter Expression

A hand-written parser for a boolean filter grammar like
`status = active AND (age > 30 OR role = admin)`, built from mutually recursive
functions that mirror the grammar's precedence levels, producing an AST you can
evaluate against a record. Because `parseFactor` recurses back into `parseExpr` on
an open parenthesis, the parser carries a depth counter so adversarially deep
parentheses are rejected rather than overflowing the stack.

This module is fully self-contained: its own `go mod init`, scanner, parser, and
evaluator inline, its own demo and tests.

## What you'll build

```text
filterparser/              independent module: example.com/filterparser
  go.mod                   go 1.26
  filter.go                scanner, Node AST, Parse/ParseWithLimit, Eval; ErrSyntax, ErrTooDeep
  filter_test.go           AST shape, precedence, errors, depth cap, eval round-trip
  cmd/
    demo/
      main.go              parse a filter, evaluate it against two records
```

- Files: `filter.go`, `cmd/demo/main.go`, `filter_test.go`.
- Implement: a scanner and a recursive-descent parser (`parseExpr`/`parseTerm`/`parseFactor`) producing a `Node` AST, with `Parse(string)` and `ParseWithLimit(string, maxDepth)`, plus `Eval(Node, map[string]string) bool`. Errors wrap `ErrSyntax`; deep nesting returns `ErrTooDeep`.
- Test: valid expressions asserting AST shape and precedence (AND tighter than OR, parentheses override); error cases (unbalanced parens, trailing tokens, empty input, missing operator); a depth-cap rejection; an `Eval` round-trip.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/04-recursive-descent-filter-parser/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/04-recursive-descent-filter-parser
```

### The grammar and why one function per level

The filter language has three precedence levels. `OR` binds loosest, `AND` binds
tighter, and a comparison or a parenthesized subexpression binds tightest:

```text
expr   := term (OR term)*
term   := factor (AND factor)*
factor := field op value | '(' expr ')'
```

Recursive descent implements this by writing one function per grammar rule, and
the call structure is what encodes precedence. `parseExpr` parses a `term`, then
while it sees `OR` it parses more terms and folds them into `Or` nodes. Each
`term` is itself an `AND`-fold of `factor`s parsed by `parseTerm`. Because a whole
`term` (an entire `AND` chain) is parsed before `parseExpr` ever considers `OR`,
`AND` automatically binds tighter than `OR` — the precedence falls out of the call
order, with no precedence table. `parseFactor` handles the atoms: a bare
comparison `field op value`, or a parenthesized `expr`, which is where the mutual
recursion closes the loop — `parseFactor` calls `parseExpr` again inside
parentheses.

That recursion back into `parseExpr` is exactly the stack-safety concern. Input
like `((((((a=b))))))` drives `parseExpr` as deep as the parentheses nest, so a
parser exposed to untrusted filter strings must bound the depth. `parseExpr`
increments a counter on entry and returns `ErrTooDeep` past the cap — the same
principle as the JSON guard, applied to a hand-written recursive parser. Without
it, a query API that accepts filter strings is a stack-overflow DoS.

The scanner turns the raw string into a token slice first, so the parser works on
tokens rather than runes. It reads runs of letters and digits as field/value
tokens (`status`, `active`, `30`), recognizes the keywords `AND` and `OR`, the
comparison operators `=`, `>`, `<`, and the parentheses. Anything else is a scan
error wrapping `ErrSyntax`.

The AST is three node types behind a marker interface, so a type switch in `Eval`
is exhaustive and `reflect.DeepEqual` can assert exact tree shape in tests. `Eval`
walks the tree: `And`/`Or` short-circuit as expected, and a `Comparison` looks up
the field in the record — string equality for `=`, numeric comparison for `>`/`<`
(a non-numeric operand makes the comparison false rather than erroring, which is
the forgiving behavior a filter endpoint wants).

Create `filter.go`:

```go
package filter

import (
	"errors"
	"fmt"
	"strconv"
	"unicode"
)

// Sentinel errors. Parse failures wrap ErrSyntax; over-deep nesting is ErrTooDeep.
var (
	ErrSyntax  = errors.New("filter syntax error")
	ErrTooDeep = errors.New("filter nesting exceeds max depth")
)

// Node is a parsed filter expression.
type Node interface{ isNode() }

// And is a logical conjunction of two subexpressions.
type And struct{ Left, Right Node }

// Or is a logical disjunction of two subexpressions.
type Or struct{ Left, Right Node }

// Comparison is a single field/op/value predicate.
type Comparison struct {
	Field string
	Op    string
	Value string
}

func (And) isNode()        {}
func (Or) isNode()         {}
func (Comparison) isNode() {}

type tokKind int

const (
	tokField tokKind = iota
	tokOp
	tokAnd
	tokOr
	tokLParen
	tokRParen
	tokEOF
)

type token struct {
	kind tokKind
	text string
}

func scan(input string) ([]token, error) {
	var toks []token
	runes := []rune(input)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			toks = append(toks, token{kind: tokLParen, text: "("})
			i++
		case r == ')':
			toks = append(toks, token{kind: tokRParen, text: ")"})
			i++
		case r == '=' || r == '>' || r == '<':
			toks = append(toks, token{kind: tokOp, text: string(r)})
			i++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) {
				j++
			}
			word := string(runes[i:j])
			switch word {
			case "AND":
				toks = append(toks, token{kind: tokAnd, text: word})
			case "OR":
				toks = append(toks, token{kind: tokOr, text: word})
			default:
				toks = append(toks, token{kind: tokField, text: word})
			}
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q: %w", string(r), ErrSyntax)
		}
	}
	return toks, nil
}

type parser struct {
	toks     []token
	pos      int
	depth    int
	maxDepth int
}

func (p *parser) peek() token {
	if p.pos >= len(p.toks) {
		return token{kind: tokEOF, text: ""}
	}
	return p.toks[p.pos]
}

func (p *parser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) parseExpr() (Node, error) {
	p.depth++
	if p.depth > p.maxDepth {
		return nil, ErrTooDeep
	}
	defer func() { p.depth-- }()

	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOr {
		p.next()
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = Or{Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseTerm() (Node, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokAnd {
		p.next()
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = And{Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseFactor() (Node, error) {
	t := p.peek()
	if t.kind == tokLParen {
		p.next()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("expected ')': %w", ErrSyntax)
		}
		p.next()
		return inner, nil
	}
	if t.kind != tokField {
		return nil, fmt.Errorf("expected a field, got %q: %w", t.text, ErrSyntax)
	}
	field := p.next().text

	op := p.peek()
	if op.kind != tokOp {
		return nil, fmt.Errorf("expected an operator after %q: %w", field, ErrSyntax)
	}
	p.next()

	val := p.peek()
	if val.kind != tokField {
		return nil, fmt.Errorf("expected a value after %q %q: %w", field, op.text, ErrSyntax)
	}
	p.next()

	return Comparison{Field: field, Op: op.text, Value: val.text}, nil
}

// Parse parses input with a default depth cap of 64.
func Parse(input string) (Node, error) {
	return ParseWithLimit(input, 64)
}

// ParseWithLimit parses input, rejecting expressions nested deeper than maxDepth
// parenthesized levels with ErrTooDeep.
func ParseWithLimit(input string, maxDepth int) (Node, error) {
	toks, err := scan(input)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty expression: %w", ErrSyntax)
	}
	p := &parser{toks: toks, maxDepth: maxDepth}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("unexpected trailing token %q: %w", p.peek().text, ErrSyntax)
	}
	return node, nil
}

// Eval evaluates a parsed filter against a record.
func Eval(n Node, record map[string]string) bool {
	switch node := n.(type) {
	case And:
		return Eval(node.Left, record) && Eval(node.Right, record)
	case Or:
		return Eval(node.Left, record) || Eval(node.Right, record)
	case Comparison:
		return evalComparison(node, record)
	default:
		return false
	}
}

func evalComparison(c Comparison, record map[string]string) bool {
	got, ok := record[c.Field]
	if !ok {
		return false
	}
	switch c.Op {
	case "=":
		return got == c.Value
	case ">", "<":
		a, err1 := strconv.Atoi(got)
		b, err2 := strconv.Atoi(c.Value)
		if err1 != nil || err2 != nil {
			return false
		}
		if c.Op == ">" {
			return a > b
		}
		return a < b
	default:
		return false
	}
}
```

### The runnable demo

The demo parses a realistic access filter and evaluates it against two user
records, showing that the AST distinguishes who matches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/filterparser"
)

func main() {
	ast, err := filter.Parse("status = active AND (age > 30 OR role = admin)")
	if err != nil {
		panic(err)
	}

	alice := map[string]string{"status": "active", "age": "42", "role": "user"}
	bob := map[string]string{"status": "active", "age": "25", "role": "user"}

	fmt.Printf("alice matches: %v\n", filter.Eval(ast, alice))
	fmt.Printf("bob matches: %v\n", filter.Eval(ast, bob))

	_, err = filter.ParseWithLimit("((((((a = b))))))", 3)
	fmt.Printf("deep parse rejected: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice matches: true
bob matches: false
deep parse rejected: filter nesting exceeds max depth
```

### Tests

`TestParsesPrecedence` asserts the exact AST for a mixed expression, proving `AND`
binds tighter than `OR`, and a second case proving parentheses override that.
`TestParseErrors` drives the malformed inputs, each expected to wrap `ErrSyntax`.
`TestDepthCap` confirms deep parentheses return `ErrTooDeep`. `TestEval` is the
round-trip: parse, evaluate against records, and check the boolean result.

Create `filter_test.go`:

```go
package filter

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestParsesPrecedence(t *testing.T) {
	t.Parallel()

	// AND binds tighter than OR: a OR b AND c parses as a OR (b AND c).
	got, err := Parse("role = user OR age > 30 AND status = active")
	if err != nil {
		t.Fatal(err)
	}
	want := Or{
		Left: Comparison{Field: "role", Op: "=", Value: "user"},
		Right: And{
			Left:  Comparison{Field: "age", Op: ">", Value: "30"},
			Right: Comparison{Field: "status", Op: "=", Value: "active"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParenthesesOverridePrecedence(t *testing.T) {
	t.Parallel()

	got, err := Parse("(role = user OR age > 30) AND status = active")
	if err != nil {
		t.Fatal(err)
	}
	want := And{
		Left: Or{
			Left:  Comparison{Field: "role", Op: "=", Value: "user"},
			Right: Comparison{Field: "age", Op: ">", Value: "30"},
		},
		Right: Comparison{Field: "status", Op: "=", Value: "active"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"unbalanced open paren", "(a = b"},
		{"unbalanced close paren", "a = b)"},
		{"missing operator", "a b"},
		{"missing value", "a ="},
		{"trailing tokens", "a = b c = d"},
		{"bad character", "a = b & c = d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tt.input)
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("err = %v, want ErrSyntax", err)
			}
		})
	}
}

func TestDepthCap(t *testing.T) {
	t.Parallel()

	_, err := ParseWithLimit("((((((a = b))))))", 3)
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("err = %v, want ErrTooDeep", err)
	}

	if _, err := ParseWithLimit("(a = b)", 3); err != nil {
		t.Fatalf("shallow expression should parse, got %v", err)
	}
}

func TestEval(t *testing.T) {
	t.Parallel()

	ast, err := Parse("status = active AND (age > 30 OR role = admin)")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		record map[string]string
		want   bool
	}{
		{"active over 30", map[string]string{"status": "active", "age": "42", "role": "user"}, true},
		{"active admin under 30", map[string]string{"status": "active", "age": "25", "role": "admin"}, true},
		{"active young user", map[string]string{"status": "active", "age": "25", "role": "user"}, false},
		{"inactive", map[string]string{"status": "banned", "age": "99", "role": "admin"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Eval(ast, tt.record); got != tt.want {
				t.Fatalf("Eval = %v, want %v", got, tt.want)
			}
		})
	}
}

func Example() {
	ast, _ := Parse("age > 18 AND status = active")
	record := map[string]string{"age": "21", "status": "active"}
	fmt.Println(Eval(ast, record))
	// Output: true
}
```

## Review

The parser is correct when its AST reflects the grammar's precedence without a
precedence table: `TestParsesPrecedence` proves `AND` binds tighter than `OR`
purely from the call structure, and the parentheses test proves the override. The
error tests pin every failure onto a wrapped `ErrSyntax`, so a caller can return a
clean `400` with the message rather than a stack trace. The depth cap is the
security point: `parseFactor` recurses into `parseExpr` on `(`, so untrusted filter
strings can nest arbitrarily deep, and `TestDepthCap` confirms the counter rejects
them with `ErrTooDeep` before the call stack does. The forgiving numeric
comparison — false rather than error on a non-numeric operand — is a deliberate
filter-endpoint choice; a stricter API would surface a typed error instead.

## Resources

- [unicode package (IsLetter, IsDigit, IsSpace)](https://pkg.go.dev/unicode)
- [strconv package (Atoi)](https://pkg.go.dev/strconv#Atoi)
- [fmt.Errorf and %w error wrapping](https://pkg.go.dev/fmt#Errorf)
- [Rob Pike: Lexical Scanning in Go](https://go.dev/talks/2011/lex.slide)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-untrusted-json-depth-guard.md](03-untrusted-json-depth-guard.md) | Next: [05-dependency-cycle-detection-dfs.md](05-dependency-cycle-detection-dfs.md)
