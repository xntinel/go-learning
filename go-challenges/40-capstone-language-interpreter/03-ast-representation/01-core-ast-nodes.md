# Exercise 1: Core AST Nodes

Before anything can read or rewrite a Monkey program, the program needs a shape in memory. This exercise builds that shape: the closed interface hierarchy (`Node`, `Statement`, `Expression`) tied together by unexported marker methods, a `Position` that travels with every node for diagnostics, and the full set of concrete expression and statement nodes — literals, identifiers, prefix and infix operators, `if`, function literals, calls, `let`, `return`, blocks, and the `Program` root. Every node carries a canonical `String()` form that renders the subtree back to fully parenthesized, source-like text, which is the single tool that makes the parser's output testable with a string comparison.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ast.go               Position, Node, Statement, Expression (marker interfaces)
nodes.go             every concrete node + its constructor and String()
cmd/
  demo/
    main.go          build a let-statement AST and print its canonical form
ast_test.go          String() assertions across literals, infix, let, program
```

- Files: `ast.go`, `nodes.go`, `cmd/demo/main.go`, `ast_test.go`.
- Implement: the `Node`/`Statement`/`Expression` interfaces and `Position` in `ast.go`; in `nodes.go` the concrete nodes `IntegerLiteral`, `BooleanLiteral`, `StringLiteral`, `Identifier`, `PrefixExpression`, `InfixExpression`, `IfExpression`, `FunctionLiteral`, `CallExpression`, `LetStatement`, `ReturnStatement`, `ExpressionStatement`, `BlockStatement`, and `Program`, each with its constructor (where applicable) and `String()`.
- Test: `ast_test.go` asserts the canonical string form of integer and boolean literals, an infix expression, a let statement, a whole program, and a return statement.
- Verify: `go test -run 'TestIntegerLiteral|TestBooleanLiteral|TestInfix|TestLet|TestProgram|TestReturn' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/03-ast-representation/01-core-ast-nodes/cmd/demo && cd go-solutions/40-capstone-language-interpreter/03-ast-representation/01-core-ast-nodes
```

### Why marker interfaces, positions, and a canonical string form

The three interfaces are the spine of the package. `Node` is the base every node satisfies, and it holds the operations meaningful for any node: `TokenLiteral()` returns the literal text of the token that started the node, `String()` returns the canonical rendering, and `Pos()` returns the source span. `Statement` and `Expression` each embed `Node` and add one unexported marker method — `statementNode()` and `expressionNode()` respectively. Those methods have empty bodies and exist only to partition the node types into two roles, and because they are unexported, only this package can implement them. That is what makes the hierarchy *closed*: no outside caller can invent a type that passes a `Statement` check, and a function that accepts a `Statement` can never be handed an expression. `go/ast` ties `ast.Node`, `ast.Stmt`, and `ast.Expr` together with the exact same technique.

`Position` records a full span — start line and column, end line and column, and an optional file — and every concrete node stores one in an unexported `pos` field returned through `Pos()`. The field is unexported on purpose: it forces construction through a constructor that takes the position as an argument, so you cannot accidentally build a node with a bare struct literal and leave its position zero. The cost is a few integers per node; the payoff is that any later phase can print `file:line:column` diagnostics for free, because the location rode along with the node from the moment the parser made it.

`String()` is the workhorse for testing. It renders the subtree back to source-like text under one firm convention: every compound expression is fully parenthesized with single spaces around operators. An `InfixExpression` is always `(left op right)`, a `PrefixExpression` always `(op right)`, so the precedence the parser chose becomes visible text — `2 + 3 * 4` prints as `(2 + (3 * 4))`. Literals print their value, an identifier its name, and a `let` as `let x = <value>;`. Because whitespace and parenthesization are fixed, one string comparison pins down the entire shape of a subtree, which is why parser tests assert on `node.String()` rather than walking fields by hand. Note the deliberate roles: `BlockStatement` implements `statementNode()` (a block lives in statement position), and the fields that hold blocks are typed `*BlockStatement` directly, never as `Expression`, so the type system keeps blocks out of expression slots. `Program` implements neither marker — a whole program is a slice of statements, not a single statement or expression.

Create `ast.go`:

```go
// ast.go
package ast

import "fmt"

// Position records the complete source span of a node.
type Position struct {
	File      string
	Line      int
	Column    int
	EndLine   int
	EndColumn int
}

// String returns a short human-readable position for error messages.
func (p Position) String() string {
	if p.File != "" {
		return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
	}
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}

// Node is the base interface that every AST node must implement.
type Node interface {
	// TokenLiteral returns the literal text of the token that initiated this node.
	TokenLiteral() string
	// String returns a canonical textual representation used for printing and
	// debugging. It is not guaranteed to re-parse to the same AST.
	String() string
	// Pos returns the source span of this node.
	Pos() Position
}

// Statement is a node that forms a complete statement. The unexported marker
// method prevents types outside this package from satisfying the interface.
type Statement interface {
	Node
	statementNode()
}

// Expression is a node that produces a value when evaluated.
type Expression interface {
	Node
	expressionNode()
}
```

The marker methods `statementNode` and `expressionNode` are Go's substitute for sum types: they make the set of implementors closed to the package.

Create `nodes.go`:

```go
// nodes.go
package ast

import (
	"bytes"
	"fmt"
	"strings"
)

// ---- Literal expressions ----

// IntegerLiteral holds a 64-bit signed integer constant.
type IntegerLiteral struct {
	Token string
	Value int64
	pos   Position
}

func (n *IntegerLiteral) expressionNode()      {}
func (n *IntegerLiteral) TokenLiteral() string { return n.Token }
func (n *IntegerLiteral) String() string       { return fmt.Sprintf("%d", n.Value) }
func (n *IntegerLiteral) Pos() Position        { return n.pos }

// NewIntegerLiteral constructs an IntegerLiteral at p.
func NewIntegerLiteral(tok string, v int64, p Position) *IntegerLiteral {
	return &IntegerLiteral{Token: tok, Value: v, pos: p}
}

// BooleanLiteral holds a boolean constant.
type BooleanLiteral struct {
	Token string
	Value bool
	pos   Position
}

func (n *BooleanLiteral) expressionNode()      {}
func (n *BooleanLiteral) TokenLiteral() string { return n.Token }
func (n *BooleanLiteral) String() string {
	if n.Value {
		return "true"
	}
	return "false"
}
func (n *BooleanLiteral) Pos() Position { return n.pos }

// NewBooleanLiteral constructs a BooleanLiteral.
func NewBooleanLiteral(tok string, v bool, p Position) *BooleanLiteral {
	return &BooleanLiteral{Token: tok, Value: v, pos: p}
}

// StringLiteral holds a string constant.
type StringLiteral struct {
	Token string
	Value string
	pos   Position
}

func (n *StringLiteral) expressionNode()      {}
func (n *StringLiteral) TokenLiteral() string { return n.Token }
func (n *StringLiteral) String() string       { return fmt.Sprintf("%q", n.Value) }
func (n *StringLiteral) Pos() Position        { return n.pos }

// NewStringLiteral constructs a StringLiteral.
func NewStringLiteral(tok, v string, p Position) *StringLiteral {
	return &StringLiteral{Token: tok, Value: v, pos: p}
}

// ---- Identifier ----

// Identifier holds a variable name used as an expression.
type Identifier struct {
	Token string
	Value string
	pos   Position
}

func (n *Identifier) expressionNode()      {}
func (n *Identifier) TokenLiteral() string { return n.Token }
func (n *Identifier) String() string       { return n.Value }
func (n *Identifier) Pos() Position        { return n.pos }

// NewIdentifier constructs an Identifier.
func NewIdentifier(tok, v string, p Position) *Identifier {
	return &Identifier{Token: tok, Value: v, pos: p}
}

// ---- Compound expressions ----

// PrefixExpression is a unary operator applied to one operand: !x, -y.
type PrefixExpression struct {
	Token    string
	Operator string
	Right    Expression
	pos      Position
}

func (n *PrefixExpression) expressionNode()      {}
func (n *PrefixExpression) TokenLiteral() string { return n.Token }
func (n *PrefixExpression) String() string {
	return fmt.Sprintf("(%s%s)", n.Operator, n.Right.String())
}
func (n *PrefixExpression) Pos() Position { return n.pos }

// InfixExpression is a binary operation: left op right.
type InfixExpression struct {
	Token    string
	Left     Expression
	Operator string
	Right    Expression
	pos      Position
}

func (n *InfixExpression) expressionNode()      {}
func (n *InfixExpression) TokenLiteral() string { return n.Token }
func (n *InfixExpression) String() string {
	return fmt.Sprintf("(%s %s %s)", n.Left.String(), n.Operator, n.Right.String())
}
func (n *InfixExpression) Pos() Position { return n.pos }

// IfExpression is: if (condition) { consequence } else { alternative }.
// Alternative is nil when there is no else branch.
type IfExpression struct {
	Token       string
	Condition   Expression
	Consequence *BlockStatement
	Alternative *BlockStatement
	pos         Position
}

func (n *IfExpression) expressionNode()      {}
func (n *IfExpression) TokenLiteral() string { return n.Token }
func (n *IfExpression) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "if %s %s", n.Condition.String(), n.Consequence.String())
	if n.Alternative != nil {
		fmt.Fprintf(&b, " else %s", n.Alternative.String())
	}
	return b.String()
}
func (n *IfExpression) Pos() Position { return n.pos }

// FunctionLiteral is: fn(params) { body }.
// Name is set when the function is assigned to a let binding.
type FunctionLiteral struct {
	Token      string
	Parameters []*Identifier
	Body       *BlockStatement
	Name       string
	pos        Position
}

func (n *FunctionLiteral) expressionNode()      {}
func (n *FunctionLiteral) TokenLiteral() string { return n.Token }
func (n *FunctionLiteral) String() string {
	var b bytes.Buffer
	params := make([]string, len(n.Parameters))
	for i, p := range n.Parameters {
		params[i] = p.String()
	}
	b.WriteString("fn")
	if n.Name != "" {
		fmt.Fprintf(&b, "<%s>", n.Name)
	}
	fmt.Fprintf(&b, "(%s) %s", strings.Join(params, ", "), n.Body.String())
	return b.String()
}
func (n *FunctionLiteral) Pos() Position { return n.pos }

// CallExpression is: function(arg1, arg2, ...).
type CallExpression struct {
	Token     string
	Function  Expression
	Arguments []Expression
	pos       Position
}

func (n *CallExpression) expressionNode()      {}
func (n *CallExpression) TokenLiteral() string { return n.Token }
func (n *CallExpression) String() string {
	args := make([]string, len(n.Arguments))
	for i, a := range n.Arguments {
		args[i] = a.String()
	}
	return fmt.Sprintf("%s(%s)", n.Function.String(), strings.Join(args, ", "))
}
func (n *CallExpression) Pos() Position { return n.pos }

// ---- Statements ----

// LetStatement is: let <name> = <value>;
type LetStatement struct {
	Token string
	Name  *Identifier
	Value Expression
	pos   Position
}

func (n *LetStatement) statementNode()       {}
func (n *LetStatement) TokenLiteral() string { return n.Token }
func (n *LetStatement) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "let %s = ", n.Name.String())
	if n.Value != nil {
		b.WriteString(n.Value.String())
	}
	b.WriteByte(';')
	return b.String()
}
func (n *LetStatement) Pos() Position { return n.pos }

// ReturnStatement is: return <value>;
type ReturnStatement struct {
	Token       string
	ReturnValue Expression
	pos         Position
}

func (n *ReturnStatement) statementNode()       {}
func (n *ReturnStatement) TokenLiteral() string { return n.Token }
func (n *ReturnStatement) String() string {
	var b bytes.Buffer
	b.WriteString("return")
	if n.ReturnValue != nil {
		fmt.Fprintf(&b, " %s", n.ReturnValue.String())
	}
	b.WriteByte(';')
	return b.String()
}
func (n *ReturnStatement) Pos() Position { return n.pos }

// ExpressionStatement wraps an expression used as a statement.
type ExpressionStatement struct {
	Token      string
	Expression Expression
	pos        Position
}

func (n *ExpressionStatement) statementNode()       {}
func (n *ExpressionStatement) TokenLiteral() string { return n.Token }
func (n *ExpressionStatement) String() string {
	if n.Expression != nil {
		return n.Expression.String()
	}
	return ""
}
func (n *ExpressionStatement) Pos() Position { return n.pos }

// BlockStatement is a sequence of statements enclosed in braces.
type BlockStatement struct {
	Token      string
	Statements []Statement
	pos        Position
}

func (n *BlockStatement) statementNode()       {}
func (n *BlockStatement) TokenLiteral() string { return n.Token }
func (n *BlockStatement) String() string {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, s := range n.Statements {
		b.WriteString(s.String())
	}
	b.WriteByte('}')
	return b.String()
}
func (n *BlockStatement) Pos() Position { return n.pos }

// Program is the root node of every AST produced by the parser.
type Program struct {
	Statements []Statement
}

func (p *Program) TokenLiteral() string {
	if len(p.Statements) > 0 {
		return p.Statements[0].TokenLiteral()
	}
	return ""
}
func (p *Program) String() string {
	var b bytes.Buffer
	for _, s := range p.Statements {
		b.WriteString(s.String())
	}
	return b.String()
}
func (p *Program) Pos() Position { return Position{} }
```

### The runnable demo

The demo builds the tree for `let result = (2 + 3) * 4;` by hand — the way the parser would assemble it — and prints its canonical form. Because every infix node parenthesizes itself, the printed string makes the nesting explicit, and the `Position` rendering shows the diagnostic form a real error message would use.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/core-ast-nodes"
)

func main() {
	// Build: let result = (2 + 3) * 4;
	prog := &ast.Program{
		Statements: []ast.Statement{
			&ast.LetStatement{
				Token: "let",
				Name:  ast.NewIdentifier("result", "result", ast.Position{}),
				Value: &ast.InfixExpression{
					Token: "*",
					Left: &ast.InfixExpression{
						Token:    "+",
						Left:     ast.NewIntegerLiteral("2", 2, ast.Position{}),
						Operator: "+",
						Right:    ast.NewIntegerLiteral("3", 3, ast.Position{}),
					},
					Operator: "*",
					Right:    ast.NewIntegerLiteral("4", 4, ast.Position{}),
				},
			},
		},
	}

	fmt.Println("Canonical form:")
	fmt.Println(prog.String())

	cond := ast.NewIdentifier("x", "x", ast.Position{File: "demo.mk", Line: 7, Column: 12})
	fmt.Printf("Identifier %q declared at %s\n", cond.Value, cond.Pos())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Canonical form:
let result = ((2 + 3) * 4);
Identifier "x" declared at demo.mk:7:12
```

### Tests

The tests assert the canonical string form of representative nodes. Because the form is deterministic, each test is a single equality check that pins the entire rendering — including parenthesization and spacing — of the subtree it builds.

Create `ast_test.go`:

```go
// ast_test.go
package ast

import "testing"

func TestIntegerLiteralString(t *testing.T) {
	t.Parallel()
	n := NewIntegerLiteral("42", 42, Position{})
	if got := n.String(); got != "42" {
		t.Fatalf("String() = %q, want %q", got, "42")
	}
}

func TestBooleanLiteralString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val  bool
		want string
	}{
		{true, "true"},
		{false, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			n := NewBooleanLiteral(tc.want, tc.val, Position{})
			if got := n.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInfixExpressionString(t *testing.T) {
	t.Parallel()
	expr := &InfixExpression{
		Token:    "+",
		Left:     NewIntegerLiteral("5", 5, Position{}),
		Operator: "+",
		Right:    NewIntegerLiteral("3", 3, Position{}),
	}
	const want = "(5 + 3)"
	if got := expr.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestLetStatementString(t *testing.T) {
	t.Parallel()
	stmt := &LetStatement{
		Token: "let",
		Name:  NewIdentifier("x", "x", Position{}),
		Value: NewIntegerLiteral("5", 5, Position{}),
	}
	const want = "let x = 5;"
	if got := stmt.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestReturnStatementString(t *testing.T) {
	t.Parallel()
	stmt := &ReturnStatement{
		Token:       "return",
		ReturnValue: NewBooleanLiteral("true", true, Position{}),
	}
	const want = "return true;"
	if got := stmt.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestProgramString(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: &InfixExpression{
					Token:    "+",
					Left:     NewIntegerLiteral("5", 5, Position{}),
					Operator: "+",
					Right:    NewIntegerLiteral("3", 3, Position{}),
				},
			},
		},
	}
	const want = "let x = (5 + 3);"
	if got := prog.String(); got != want {
		t.Fatalf("prog.String() = %q, want %q", got, want)
	}
}
```

## Review

The module is correct when a hand-built tree renders to exactly the canonical string you expect, parenthesization and spacing included. Confirm that an `InfixExpression` always prints as `(left op right)`, that a nested infix such as `(2 + 3) * 4` prints as `((2 + 3) * 4)` so precedence is visible, and that a `let` ends in a semicolon. Check that the interfaces hold their line: `Statement` and `Expression` are satisfied only by the package's own types because the marker methods are unexported, `BlockStatement` is a `Statement` (not an `Expression`), and `Program` is a plain `Node`. The position machinery is right when a node built with a non-zero `Position` renders as `file:line:column`, proving the location traveled with the node from construction.

A common pitfall is implementing the wrong marker method — giving a statement an `expressionNode()` — which compiles but lets the node slip into expression slots the evaluator cannot handle; implement only the marker that matches the node's role. Another is exporting the `pos` field and building nodes with bare struct literals, which makes a forgotten position easy; keeping `pos` unexported and routing through the constructors prevents it.

## Resources

- [go/ast package](https://pkg.go.dev/go/ast) — the standard library's `Node`, `Stmt`, and `Expr` interfaces and the unexported marker-method technique this exercise mirrors.
- [Writing An Interpreter In Go](https://interpreterbook.com/) — the Monkey language and the AST design these node types follow.
- [Crafting Interpreters: Representing Code](https://craftinginterpreters.com/representing-code.html) — the rationale behind AST node design and explicit tree shapes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-visitor-traversal.md](02-visitor-traversal.md)
