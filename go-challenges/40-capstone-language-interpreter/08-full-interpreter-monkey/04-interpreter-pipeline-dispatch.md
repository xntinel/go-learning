# Exercise 4: Interpreter Pipeline Dispatch

The full `monkey` binary stitches the lexer, parser, evaluator, and REPL from the previous lessons into one process and routes a subcommand to the right pipeline stage. That full integration cannot compile offline — it imports five packages built across this chapter — so this exercise rebuilds the part that does: a complete, self-contained pipeline for a small arithmetic language, with subcommand dispatch over it. It is the integration architecture in miniature, source text flowing lexer to parser to evaluator to output, with `run`, `tokens`, and `ast` each exposing a different stage of the same pipeline. Everything compiles and tests offline, so the wiring discipline the real binary needs can be proven here without the rest of the interpreter.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
lexer.go            TokenType, Token, Lex (source -> token stream)
ast.go              Node, IntegerLiteral, InfixExpression, PrefixExpression
parser.go           Parse (Pratt parser: + - * /, parens, unary minus)
eval.go             Eval (tree walk -> int64)
dispatch.go         Run, TokenStream, AST, Dispatch (subcommand -> stage)
interp_test.go      arithmetic, precedence, parse errors, dispatch
cmd/
  demo/
    main.go         run one expression through all three stages
```

- Files: `lexer.go`, `ast.go`, `parser.go`, `eval.go`, `dispatch.go`, `cmd/demo/main.go`, `interp_test.go`.
- Implement: `Lex`, the three AST node types, the Pratt `Parse`, `Eval`, and the `Run` / `TokenStream` / `AST` / `Dispatch` integration layer.
- Test: `interp_test.go` checks arithmetic results and precedence, division by zero, parse errors, the S-expression rendering, and subcommand dispatch.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p interpreter-pipeline/cmd/demo && cd interpreter-pipeline
go mod init example.com/interpreter-pipeline
```

### The pipeline as composed stages

The interpreter is four stages, each with a contract so narrow it can be read in isolation: the lexer turns text into tokens, the parser turns tokens into a tree, the evaluator turns a tree into a value, and the dispatch layer chooses how far down that chain a given subcommand runs. `Lex` produces a flat slice of tokens terminated by an `EOF` marker so the parser always has a sentinel to stop on rather than a length check at every step. The parser never sees a byte of source and the evaluator never sees a token; each stage consumes only the previous stage's output. That is what lets the binary expose `tokens` (stop after stage one), `ast` (stop after stage two), and `run` (run all the way through) as three views of one pipeline rather than three separate programs.

The parser is a Pratt parser, the same design the real Monkey parser uses. Each operator carries a precedence — `+` and `-` bind loosest, `*` and `/` bind tighter, unary minus tightest — and the core loop keeps folding the right-hand side into the tree as long as the next operator binds tighter than the precedence it was called with. That single comparison, `prec <= minPrec` to stop, is what makes `2 + 3 * 4` parse as `2 + (3 * 4)` and `10 - 2 - 3` parse left-associatively as `(10 - 2) - 3`. Parentheses are handled in the prefix position by parsing a fresh sub-expression at the lowest precedence and then requiring the closing paren, which is why a missing `)` surfaces as a parse error rather than a silent truncation.

The dispatch layer is the integration point, and it is deliberately the thinnest stage. `Run` is the whole pipeline composed: parse, then evaluate. `TokenStream` and `AST` stop early. `Dispatch` maps a subcommand string to one of these and returns text, with an unknown subcommand rejected by a sentinel error rather than handled cleverly — the same exit-discipline instinct the standalone CLI parser exercise formalizes. Keeping evaluation errors (division by zero) and syntax errors (a dangling operator) as distinct wrapped sentinels means a caller can tell a malformed program from a well-formed one that failed at runtime, which is exactly the distinction the real binary turns into exit codes 1 versus 2.

Create `lexer.go`:

```go
package interp

import "fmt"

// TokenType labels a lexical token.
type TokenType string

const (
	INT     TokenType = "INT"
	PLUS    TokenType = "PLUS"
	MINUS   TokenType = "MINUS"
	STAR    TokenType = "STAR"
	SLASH   TokenType = "SLASH"
	LPAREN  TokenType = "LPAREN"
	RPAREN  TokenType = "RPAREN"
	EOF     TokenType = "EOF"
	ILLEGAL TokenType = "ILLEGAL"
)

// Token is a single lexical unit with its source literal.
type Token struct {
	Type    TokenType
	Literal string
}

func (t Token) String() string {
	return fmt.Sprintf("%-7s %q", t.Type, t.Literal)
}

// Lex turns source text into a token stream terminated by an EOF token.
func Lex(src string) []Token {
	var toks []Token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '+':
			toks = append(toks, Token{PLUS, "+"})
			i++
		case c == '-':
			toks = append(toks, Token{MINUS, "-"})
			i++
		case c == '*':
			toks = append(toks, Token{STAR, "*"})
			i++
		case c == '/':
			toks = append(toks, Token{SLASH, "/"})
			i++
		case c == '(':
			toks = append(toks, Token{LPAREN, "("})
			i++
		case c == ')':
			toks = append(toks, Token{RPAREN, ")"})
			i++
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			toks = append(toks, Token{INT, src[i:j]})
			i = j
		default:
			toks = append(toks, Token{ILLEGAL, string(c)})
			i++
		}
	}
	toks = append(toks, Token{EOF, ""})
	return toks
}
```

The AST is three node types behind one interface. `String` renders each as a fully parenthesized S-expression, which is both the `ast` subcommand's output and the cleanest possible assertion target for a test: a tree's shape becomes a string you can compare exactly.

Create `ast.go`:

```go
package interp

import (
	"fmt"
	"strconv"
)

// Node is an expression in the parsed syntax tree.
type Node interface {
	// String renders the node as a fully parenthesized S-expression.
	String() string
}

// IntegerLiteral is a single integer constant.
type IntegerLiteral struct {
	Value int64
}

func (n *IntegerLiteral) String() string { return strconv.FormatInt(n.Value, 10) }

// InfixExpression is a binary operation: Left Op Right.
type InfixExpression struct {
	Op    string
	Left  Node
	Right Node
}

func (n *InfixExpression) String() string {
	return fmt.Sprintf("(%s %s %s)", n.Op, n.Left.String(), n.Right.String())
}

// PrefixExpression is a unary operation, such as negation.
type PrefixExpression struct {
	Op    string
	Right Node
}

func (n *PrefixExpression) String() string {
	return fmt.Sprintf("(%s %s)", n.Op, n.Right.String())
}
```

The parser folds tokens into that tree by precedence. The `precedences` map is the operator table; `parseExpr(minPrec)` keeps absorbing operators that bind tighter than `minPrec` and recurses for the right operand, which is the whole of Pratt parsing for binary operators. The prefix handler deals with integers, a leading minus, and a parenthesized sub-expression.

Create `parser.go`:

```go
package interp

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrParse is returned for any syntactic error.
var ErrParse = errors.New("parse error")

const (
	lowest  = iota
	sum     // + -
	product // * /
	prefix  // unary -
)

var precedences = map[TokenType]int{
	PLUS:  sum,
	MINUS: sum,
	STAR:  product,
	SLASH: product,
}

type parser struct {
	toks []Token
	pos  int
}

// Parse builds the syntax tree for a single arithmetic expression.
func Parse(src string) (Node, error) {
	p := &parser{toks: Lex(src)}
	node, err := p.parseExpr(lowest)
	if err != nil {
		return nil, err
	}
	if p.cur().Type != EOF {
		return nil, fmt.Errorf("%w: unexpected %s", ErrParse, p.cur())
	}
	return node, nil
}

func (p *parser) cur() Token  { return p.toks[p.pos] }
func (p *parser) next() Token { p.pos++; return p.toks[p.pos-1] }

func (p *parser) parseExpr(minPrec int) (Node, error) {
	left, err := p.parsePrefix()
	if err != nil {
		return nil, err
	}
	for {
		prec, ok := precedences[p.cur().Type]
		if !ok || prec <= minPrec {
			break
		}
		op := p.next()
		right, err := p.parseExpr(prec)
		if err != nil {
			return nil, err
		}
		left = &InfixExpression{Op: op.Literal, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parsePrefix() (Node, error) {
	switch p.cur().Type {
	case INT:
		tok := p.next()
		v, err := strconv.ParseInt(tok.Literal, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: bad integer %q", ErrParse, tok.Literal)
		}
		return &IntegerLiteral{Value: v}, nil
	case MINUS:
		p.next()
		right, err := p.parseExpr(prefix)
		if err != nil {
			return nil, err
		}
		return &PrefixExpression{Op: "-", Right: right}, nil
	case LPAREN:
		p.next()
		node, err := p.parseExpr(lowest)
		if err != nil {
			return nil, err
		}
		if p.cur().Type != RPAREN {
			return nil, fmt.Errorf("%w: expected ) got %s", ErrParse, p.cur())
		}
		p.next()
		return node, nil
	default:
		return nil, fmt.Errorf("%w: unexpected %s", ErrParse, p.cur())
	}
}
```

The evaluator is a single recursive type switch over the three node types. The only runtime error it can raise is division by zero, kept as its own sentinel so the dispatch layer can tell a runtime failure apart from a syntax failure.

Create `eval.go`:

```go
package interp

import (
	"errors"
	"fmt"
)

// ErrDivByZero is returned when evaluation divides by zero.
var ErrDivByZero = errors.New("division by zero")

// Eval walks the syntax tree and returns the integer result.
func Eval(node Node) (int64, error) {
	switch n := node.(type) {
	case *IntegerLiteral:
		return n.Value, nil
	case *PrefixExpression:
		v, err := Eval(n.Right)
		if err != nil {
			return 0, err
		}
		return -v, nil
	case *InfixExpression:
		l, err := Eval(n.Left)
		if err != nil {
			return 0, err
		}
		r, err := Eval(n.Right)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case "+":
			return l + r, nil
		case "-":
			return l - r, nil
		case "*":
			return l * r, nil
		case "/":
			if r == 0 {
				return 0, ErrDivByZero
			}
			return l / r, nil
		default:
			return 0, fmt.Errorf("unknown operator %q", n.Op)
		}
	default:
		return 0, fmt.Errorf("unknown node %T", node)
	}
}
```

The dispatch layer composes the stages and routes a subcommand to one of them, the integration that the rest of the file exists to support.

Create `dispatch.go`:

```go
package interp

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownCommand is returned by Dispatch for an unrecognized subcommand.
var ErrUnknownCommand = errors.New("unknown subcommand")

// Run executes the full pipeline: lex, parse, evaluate, and return the result.
func Run(src string) (int64, error) {
	node, err := Parse(src)
	if err != nil {
		return 0, err
	}
	return Eval(node)
}

// TokenStream renders the lexer output, one token per line.
func TokenStream(src string) string {
	var b strings.Builder
	for _, t := range Lex(src) {
		fmt.Fprintln(&b, t.String())
	}
	return b.String()
}

// AST renders the parsed tree as an S-expression.
func AST(src string) (string, error) {
	node, err := Parse(src)
	if err != nil {
		return "", err
	}
	return node.String(), nil
}

// Dispatch routes a subcommand to its pipeline stage and returns the text
// output. It mirrors the monkey binary's run/tokens/ast subcommands over a
// single self-contained module.
func Dispatch(cmd, src string) (string, error) {
	switch cmd {
	case "run":
		v, err := Run(src)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d", v), nil
	case "tokens":
		return TokenStream(src), nil
	case "ast":
		return AST(src)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownCommand, cmd)
	}
}
```

### The runnable demo

The demo sends one expression through all three stages in turn — the token stream, the parsed S-expression, and the evaluated result — so the pipeline is visible end to end in a single run. `2 * (3 + 4) - 1` exercises precedence, a parenthesized group, and a trailing subtraction, which is enough to show the parser respecting both grouping and left-associativity.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	interp "example.com/interpreter-pipeline"
)

func main() {
	const src = "2 * (3 + 4) - 1"

	fmt.Print("tokens:\n", interp.TokenStream(src))

	tree, _ := interp.AST(src)
	fmt.Println("ast:", tree)

	result, _ := interp.Run(src)
	fmt.Println("run:", result)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tokens:
INT     "2"
STAR    "*"
LPAREN  "("
INT     "3"
PLUS    "+"
INT     "4"
RPAREN  ")"
MINUS   "-"
INT     "1"
EOF     ""
ast: (- (* 2 (+ 3 4)) 1)
run: 13
```

### Tests

The tests cover each stage and the integration over them. The arithmetic table pins precedence and associativity directly: `2 + 3 * 4` must be 14 and `10 - 2 - 3` must be 5, which only holds if the Pratt loop and its `prec <= minPrec` stop condition are right. The error tests separate the two failure classes — a syntax error reaches `ErrParse`, a division by zero reaches `ErrDivByZero` — and the dispatch tests confirm a known subcommand routes to its stage while an unknown one returns `ErrUnknownCommand`. The AST test asserts the exact S-expression so a regression in tree shape is caught immediately.

Create `interp_test.go`:

```go
package interp

import (
	"errors"
	"fmt"
	"testing"
)

func TestRunArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src  string
		want int64
	}{
		{"1 + 2", 3},
		{"2 * 3 + 4", 10},
		{"2 + 3 * 4", 14},
		{"2 * (3 + 4)", 14},
		{"10 - 2 - 3", 5},
		{"-5 + 8", 3},
		{"100 / 5 / 2", 10},
	}
	for _, tc := range cases {
		got, err := Run(tc.src)
		if err != nil {
			t.Fatalf("Run(%q): %v", tc.src, err)
		}
		if got != tc.want {
			t.Errorf("Run(%q) = %d, want %d", tc.src, got, tc.want)
		}
	}
}

func TestRunDivByZero(t *testing.T) {
	t.Parallel()
	_, err := Run("1 / 0")
	if !errors.Is(err, ErrDivByZero) {
		t.Fatalf("err = %v, want ErrDivByZero", err)
	}
}

func TestParseError(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"1 +", "(1 + 2", "1 2", "* 3"} {
		if _, err := Parse(src); !errors.Is(err, ErrParse) {
			t.Errorf("Parse(%q): err = %v, want ErrParse", src, err)
		}
	}
}

func TestAST(t *testing.T) {
	t.Parallel()
	got, err := AST("2 * (3 + 4) - 1")
	if err != nil {
		t.Fatal(err)
	}
	want := "(- (* 2 (+ 3 4)) 1)"
	if got != want {
		t.Fatalf("AST = %q, want %q", got, want)
	}
}

func TestLexProducesEOF(t *testing.T) {
	t.Parallel()
	toks := Lex("1+2")
	if len(toks) == 0 || toks[len(toks)-1].Type != EOF {
		t.Fatalf("last token = %v, want EOF", toks[len(toks)-1])
	}
}

func TestDispatchRun(t *testing.T) {
	t.Parallel()
	out, err := Dispatch("run", "6 * 7")
	if err != nil {
		t.Fatal(err)
	}
	if out != "42" {
		t.Fatalf("Dispatch run = %q, want 42", out)
	}
}

func TestDispatchUnknown(t *testing.T) {
	t.Parallel()
	_, err := Dispatch("explode", "1")
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("err = %v, want ErrUnknownCommand", err)
	}
}

func ExampleRun() {
	v, _ := Run("2 * (3 + 4)")
	fmt.Println(v)
	// Output:
	// 14
}

func ExampleAST() {
	s, _ := AST("1 + 2 * 3")
	fmt.Println(s)
	// Output:
	// (+ 1 (* 2 3))
}
```

## Review

The pipeline is correct when each stage honors its contract and the dispatch over them keeps the failure classes distinct. Precedence and associativity are the parser's load-bearing behavior: `2 + 3 * 4` is 14, `2 * (3 + 4)` is 14, and `10 - 2 - 3` is 5, all of which follow from the single `prec <= minPrec` stop condition and parsing the right operand at the operator's own precedence. The `EOF` sentinel the lexer appends is what lets the parser detect trailing garbage (`1 2`) and a missing close paren rather than running off the end of the slice. Keep `ErrParse` and `ErrDivByZero` as separate wrapped sentinels so a caller — and the real binary's exit-code layer — can tell a malformed program from a well-formed one that failed at runtime, and reject unknown subcommands with `ErrUnknownCommand` instead of acting on the string. This is the same integration the full `monkey` binary performs over the chapter's lexer, parser, and evaluator; here it is small enough to compile and prove on its own.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — chapters 1-4 build the lexer, Pratt parser, AST, and evaluator this pipeline mirrors.
- [pkg.go.dev/strconv](https://pkg.go.dev/strconv) — `strconv.ParseInt` and `strconv.FormatInt` used in the parser and AST.
- [pkg.go.dev/strings](https://pkg.go.dev/strings) — `strings.Builder` used to assemble the token-stream output.
- [go.dev/blog/go1.13-errors](https://go.dev/blog/go1.13-errors) — the `%w` wrapping and `errors.Is` matching behind the distinct error classes.

---

Back to [03-cli-argument-parser.md](03-cli-argument-parser.md)
