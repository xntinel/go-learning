# Exercise 2: Visitor Traversal

A tree you cannot walk is barely a tree. This exercise adds traversal to the AST: `Walk`, which drives a depth-first pre-order visit and recurses into every child; `Inspect`, the plain-function convenience that matches `go/ast.Inspect`; and the generic `Collect[T Node]`, which gathers every node of one concrete type. The whole design follows the visitor pattern Go's standard library settled on — a single `Visit(node Node) Visitor` method, pre-order, with a `nil` return to prune a subtree — so anyone who has used `go/ast.Walk` already knows how this behaves.

This module is fully self-contained. It carries the full node definitions it needs and adds the traversal layer on top; nothing here imports any other exercise.

## What you'll build

```text
ast.go               Position, Node, Statement, Expression (marker interfaces)
nodes.go             every concrete node + its constructor and String()
visitor.go           Visitor, Walk, Inspect, Collect[T]
cmd/
  demo/
    main.go          count nodes and collect identifiers from a program
visitor_test.go      pre-order count, subtree pruning, Collect, visit order
```

- Files: `ast.go`, `nodes.go`, `visitor.go`, `cmd/demo/main.go`, `visitor_test.go`.
- Implement: the `Visitor` interface, `Walk(v Visitor, node Node)`, `Inspect(node Node, fn func(Node) bool)`, and the generic `Collect[T Node](root Node) []T` in `visitor.go`, on top of the node types from the core module.
- Test: `visitor_test.go` counts every node in pre-order, proves a `false` return prunes a subtree, collects identifiers by type, and asserts the exact pre-order visit sequence.
- Verify: `go test -run 'TestInspect|TestCollect|TestWalk|ExampleCollect' -race ./...`

Set up the module:

```bash
mkdir -p visitor-traversal/cmd/demo && cd visitor-traversal
go mod init example.com/visitor-traversal
```

### Why a single-method visitor, pre-order, with a nil return

The classic object-oriented visitor relies on method overloading — one `visit` per concrete node type — and virtual dispatch. Go has neither, so it uses a different and simpler shape, the one `go/ast` adopted: a `Visitor` is any type with one method, `Visit(node Node) Visitor`. The free function `Walk(v, node)` calls `v.Visit(node)` *before* descending, then type-switches on the node's concrete type to recurse into each child in source order. The return value of `Visit` is the control knob. Returning `nil` prunes the subtree — `Walk` does not descend into the node's children. Returning a non-nil visitor (almost always the receiver itself) continues the traversal with that visitor, which is how a traversal can hand a child subtree a different, stateful visitor without resorting to closures. This pre-order, prune-on-nil contract is exactly `go/ast.Walk`'s.

There is one deliberate simplification relative to the standard library. `go/ast.Walk`, after it finishes a node's children, calls the visitor once more with a `nil` node — a "subtree complete" signal that enables genuine post-order hooks (closing a scope, decrementing a depth counter, printing a closing bracket). The `Walk` here omits that trailing `Visit(nil)` to stay purely pre-order and easy to read. If you later need post-order behavior, the extension is mechanical: after the type-switch recurses into the children, call `v.Visit(nil)` once before returning, and have visitors treat a `nil` argument as the "leaving" signal. Knowing the omission is here means the standard library's `Visit(nil)` calls will not surprise you.

Two conveniences sit on top of `Walk`. `Inspect` wraps a plain `func(Node) bool` as a visitor — return `true` to descend, `false` to prune — matching `go/ast.Inspect`, which covers the common case where you do not need a stateful visitor object. The generic `Collect[T Node]` uses `Inspect` to gather every node whose concrete type is exactly `T`: `Collect[*Identifier](prog)` returns all identifiers in pre-order. Generics make this work because the type assertion `n.(T)` is checked against the caller's chosen type at every node. The critical correctness rule for `Walk` is completeness of the type-switch: the `default: panic(...)` arm means that adding a node type without teaching `Walk` to recurse into it fails loudly in a test rather than silently skipping that node's children.

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
	TokenLiteral() string
	String() string
	Pos() Position
}

// Statement is a node that forms a complete statement.
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

Create `nodes.go`:

```go
// nodes.go
package ast

import (
	"bytes"
	"fmt"
	"strings"
)

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

Create `visitor.go`:

```go
// visitor.go
package ast

import "fmt"

// Visitor is implemented by any type that wants to traverse an AST.
// Visit is called before a node's children are visited.
// Returning nil prunes the subtree; the children are not visited.
// Returning a Visitor (usually the receiver) continues normal traversal.
// This follows go/ast.Walk's pre-order Visit convention, with one deliberate
// simplification: go/ast.Walk also calls v.Visit(nil) after a node's children
// (to signal "subtree done"); this Walk omits that post-visit call. Add it if
// you need post-order hooks or depth tracking.
type Visitor interface {
	Visit(node Node) Visitor
}

// Walk traverses the AST rooted at node in depth-first pre-order, calling
// v.Visit before descending into each node's children.
func Walk(v Visitor, node Node) {
	if v = v.Visit(node); v == nil {
		return
	}
	switch n := node.(type) {
	case *Program:
		for _, s := range n.Statements {
			Walk(v, s)
		}
	case *LetStatement:
		Walk(v, n.Name)
		if n.Value != nil {
			Walk(v, n.Value)
		}
	case *ReturnStatement:
		if n.ReturnValue != nil {
			Walk(v, n.ReturnValue)
		}
	case *ExpressionStatement:
		if n.Expression != nil {
			Walk(v, n.Expression)
		}
	case *BlockStatement:
		for _, s := range n.Statements {
			Walk(v, s)
		}
	case *IfExpression:
		Walk(v, n.Condition)
		Walk(v, n.Consequence)
		if n.Alternative != nil {
			Walk(v, n.Alternative)
		}
	case *PrefixExpression:
		Walk(v, n.Right)
	case *InfixExpression:
		Walk(v, n.Left)
		Walk(v, n.Right)
	case *FunctionLiteral:
		for _, p := range n.Parameters {
			Walk(v, p)
		}
		Walk(v, n.Body)
	case *CallExpression:
		Walk(v, n.Function)
		for _, a := range n.Arguments {
			Walk(v, a)
		}
	case *Identifier, *IntegerLiteral, *BooleanLiteral, *StringLiteral:
		// Leaf nodes: no children to walk.
	default:
		panic(fmt.Sprintf("Walk: unhandled node type %T", node))
	}
}

// inspector wraps a plain function as a Visitor for use by Inspect.
type inspector func(Node) bool

func (f inspector) Visit(n Node) Visitor {
	if f(n) {
		return f
	}
	return nil
}

// Inspect traverses node depth-first, calling fn before each node.
// If fn returns false the subtree rooted at that node is not visited.
// This matches the signature of go/ast.Inspect.
func Inspect(node Node, fn func(Node) bool) {
	Walk(inspector(fn), node)
}

// Collect returns all nodes in the subtree rooted at root whose concrete type
// satisfies T. Nodes are returned in depth-first pre-order.
func Collect[T Node](root Node) []T {
	var result []T
	Inspect(root, func(n Node) bool {
		if t, ok := n.(T); ok {
			result = append(result, t)
		}
		return true
	})
	return result
}
```

`Collect` uses a type parameter constrained to `Node`. The type assertion `n.(T)` succeeds when the concrete type of `n` matches `T` exactly — `Collect[*Identifier]` gathers `*Identifier` nodes and nothing else.

### The runnable demo

The demo builds a small program with two statements and uses `Inspect` to count every node, then `Collect[*ast.Identifier]` to pull out the identifiers by type. The count is deterministic because traversal is pre-order and complete, so the printed totals are a fixed property of the tree shape.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/visitor-traversal"
)

func main() {
	// let x = y;
	// return x;
	prog := &ast.Program{
		Statements: []ast.Statement{
			&ast.LetStatement{
				Token: "let",
				Name:  ast.NewIdentifier("x", "x", ast.Position{}),
				Value: ast.NewIdentifier("y", "y", ast.Position{}),
			},
			&ast.ReturnStatement{
				Token:       "return",
				ReturnValue: ast.NewIdentifier("x", "x", ast.Position{}),
			},
		},
	}

	var nodes int
	ast.Inspect(prog, func(ast.Node) bool {
		nodes++
		return true
	})
	fmt.Printf("total nodes: %d\n", nodes)

	ids := ast.Collect[*ast.Identifier](prog)
	fmt.Printf("identifiers: %d\n", len(ids))
	for _, id := range ids {
		fmt.Println("  " + id.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
total nodes: 6
identifiers: 3
  x
  y
  x
```

### Tests

The tests pin the traversal contract: pre-order count, pruning on a `false` return, type-filtered collection, and the exact visit sequence. The pruning test is the important one — it proves that returning `false` from the inspect function stops descent into that node's children, not merely that it skips the node itself.

Create `visitor_test.go`:

```go
// visitor_test.go
package ast

import (
	"fmt"
	"testing"
)

func TestInspectCountsAllNodes(t *testing.T) {
	t.Parallel()
	// Program
	//   LetStatement
	//     Identifier "x"        (Name)
	//     IntegerLiteral 1      (Value)
	//   ReturnStatement
	//     Identifier "x"        (ReturnValue)
	// Total: 6 nodes visited in pre-order.
	prog := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: NewIntegerLiteral("1", 1, Position{}),
			},
			&ReturnStatement{
				Token:       "return",
				ReturnValue: NewIdentifier("x", "x", Position{}),
			},
		},
	}
	var count int
	Inspect(prog, func(Node) bool {
		count++
		return true
	})
	if count != 6 {
		t.Fatalf("Inspect visited %d nodes, want 6", count)
	}
}

func TestInspectPrunesSubtree(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: NewIntegerLiteral("1", 1, Position{}),
			},
		},
	}
	var count int
	Inspect(prog, func(n Node) bool {
		count++
		// Stop descending into LetStatement; its children must not be counted.
		_, isLet := n.(*LetStatement)
		return !isLet
	})
	// Program + LetStatement = 2; children of LetStatement are pruned.
	if count != 2 {
		t.Fatalf("Inspect visited %d nodes with pruning, want 2", count)
	}
}

func TestCollectFindsIdentifiers(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: NewIdentifier("y", "y", Position{}),
			},
		},
	}
	ids := Collect[*Identifier](prog)
	if len(ids) != 2 {
		t.Fatalf("Collect found %d identifiers, want 2", len(ids))
	}
	if ids[0].Value != "x" || ids[1].Value != "y" {
		t.Fatalf("ids = %v %v, want x y", ids[0].Value, ids[1].Value)
	}
}

func TestWalkVisitsAllNodesInOrder(t *testing.T) {
	t.Parallel()
	// Program
	//   ExpressionStatement
	//     InfixExpression
	//       IntegerLiteral 5
	//       IntegerLiteral 3
	prog := &Program{
		Statements: []Statement{
			&ExpressionStatement{
				Token: "5",
				Expression: &InfixExpression{
					Token:    "+",
					Left:     NewIntegerLiteral("5", 5, Position{}),
					Operator: "+",
					Right:    NewIntegerLiteral("3", 3, Position{}),
				},
			},
		},
	}
	var types []string
	Inspect(prog, func(n Node) bool {
		types = append(types, fmt.Sprintf("%T", n))
		return true
	})
	want := []string{
		"*ast.Program",
		"*ast.ExpressionStatement",
		"*ast.InfixExpression",
		"*ast.IntegerLiteral",
		"*ast.IntegerLiteral",
	}
	if len(types) != len(want) {
		t.Fatalf("visited %d nodes, want %d: %v", len(types), len(want), types)
	}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("node[%d] = %q, want %q", i, types[i], w)
		}
	}
}

// ExampleCollect demonstrates gathering all *Identifier nodes from a program.
func ExampleCollect() {
	prog := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: NewIdentifier("y", "y", Position{}),
			},
		},
	}
	ids := Collect[*Identifier](prog)
	fmt.Println(len(ids))
	fmt.Println(ids[0].Value)
	fmt.Println(ids[1].Value)
	// Output:
	// 2
	// x
	// y
}
```

## Review

Traversal is correct when the count is exact, pruning truly stops descent, and the visit order matches pre-order. Confirm that `Inspect` over a two-statement program with a `let` (name plus integer value) and a `return` (identifier) visits exactly six nodes, that returning `false` at the `LetStatement` yields a count of two because its children are pruned, and that the recorded type sequence for a program wrapping `5 + 3` is `Program, ExpressionStatement, InfixExpression, IntegerLiteral, IntegerLiteral`. `Collect[*Identifier]` should return only identifiers, in pre-order, and nothing else.

The pitfall that bites hardest is an incomplete `Walk` switch: add a node type and forget its case, and its children are silently never visited, so a collect or count quietly under-reports. The `default: panic` arm converts that into an immediate, located failure the first time a test walks a tree containing the new node. The second pitfall is conceptual — expecting a post-order hook from this `Walk`. It is pure pre-order; the `Visit(nil)` that `go/ast.Walk` makes after children is intentionally omitted here.

## Resources

- [go/ast.Walk](https://pkg.go.dev/go/ast#Walk) — the standard library's `Visitor` interface and pre-order walk this exercise mirrors, including the `Visit(nil)` post-visit call.
- [go/ast.Inspect](https://pkg.go.dev/go/ast#Inspect) — the plain-function traversal whose signature `Inspect` here matches.
- [Go Blog: An Introduction to Generics](https://go.dev/blog/intro-generics) — the type-parameter mechanics that make `Collect[T Node]` possible.

---

Back to [01-core-ast-nodes.md](01-core-ast-nodes.md) | Next: [03-transform-clone-equal.md](03-transform-clone-equal.md)
