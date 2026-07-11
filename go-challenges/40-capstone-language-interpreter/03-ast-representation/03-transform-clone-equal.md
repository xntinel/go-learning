# Exercise 3: Clone, Equal, and Transform

A static tree becomes a manipulable one through three primitives. `Clone` produces a fully independent deep copy so a rewrite cannot leak back into the original. `Equal` reports structural equivalence while ignoring source positions — the comparison `reflect.DeepEqual` cannot give you. `Transform` rewrites a tree bottom-up so that an optimization pass sees already-transformed children when it reaches a parent, which is exactly what makes multi-level constant folding collapse `(2 + 3) * 4` to `20` in a single traversal. This exercise builds all three plus `ConstantFold`, the example pass they exist to support.

This module is fully self-contained. It carries the full node definitions and the traversal layer it needs and adds the manipulation primitives on top; nothing here imports any other exercise.

## What you'll build

```text
ast.go               Position, Node, Statement, Expression (marker interfaces)
nodes.go             every concrete node + its constructor and String()
visitor.go           Walk, Inspect, Collect[T] (used by the demo and tests)
transform.go         Clone, Equal, Transform, ConstantFold
cmd/
  demo/
    main.go          fold (2 + 3) * 4 to 20 on a clone, original preserved
transform_test.go    clone independence, position-blind Equal, multi-level fold
```

- Files: `ast.go`, `nodes.go`, `visitor.go`, `cmd/demo/main.go`, `transform_test.go`.
- Implement: `Clone(node Node) Node`, `Equal(a, b Node) bool`, `Transform(node Node, fn func(Node) Node) Node`, and `ConstantFold() func(Node) Node` in `transform.go`, on top of the node and traversal layers.
- Test: `transform_test.go` proves a clone is independent of its source, that `Equal` ignores positions but distinguishes values, that integer and boolean constant folding work (and skip division by zero), that nested folding collapses in one pass, and that transforming a clone leaves the original untouched.
- Verify: `go test -run 'TestClone|TestEqual|TestConstantFold|TestTransform|ExampleTransform' -race ./...`

Set up the module:

```bash
mkdir -p transform-clone-equal/cmd/demo && cd transform-clone-equal
go mod init example.com/transform-clone-equal
```

### Why deep clone, position-blind equality, and bottom-up transform

`Clone` produces an independent deep copy: every node and slice is freshly allocated, so a mutation anywhere in the copy is invisible to the original. It is the prerequisite for any pass that rewrites a tree while keeping the input intact — clone first, then mutate the clone. The implementation is a type-switch that rebuilds each node from its fields and recurses into children, ending in a `default` that panics on an unknown node type, so adding a node without teaching `Clone` about it fails loudly in a test rather than silently dropping a subtree.

`Equal` exists because `reflect.DeepEqual` is wrong for this job. `reflect.DeepEqual` compares every field, including `pos`, so two trees parsed from identical source at different offsets — or an original and a clone whose synthesized nodes carry different positions — would compare unequal even though they mean the same thing. `Equal` is a hand-written recursive comparator that ignores `pos` entirely and compares only the semantic fields: node type, operator strings, literal values, names, and (recursively) children. Two integer literals with value 5 are equal regardless of where they were written. Writing it by hand also documents, in one place, exactly which fields constitute "sameness."

`Transform` rewrites bottom-up. It recurses into a node's children first, replacing each child slot with the transformed result, and only then calls `fn` on the now-already-transformed parent. If `fn` returns the same node, nothing changes; if it returns a new node, the parent's slot is updated. Bottom-up order is what makes multi-level optimization work in one pass: folding `(2 + 3) * 4` descends to `2 + 3`, folds it to the literal `5`, returns to the `* 4` node whose left child is now `5`, and folds `5 * 4` to `20`. Because children are always folded before their parent is examined, one traversal collapses the whole chain. `Transform` mutates in place, so a caller that must preserve the original writes the canonical pairing `Transform(Clone(prog), ConstantFold())`.

`ConstantFold` is the pass these primitives support. It folds integer arithmetic (`+`, `-`, `*`, `/`) and boolean operations (`&&`, `||`) whose operands are already literals, and it is careful about exactly one thing: division by zero is left un-folded, returned intact so the runtime raises the error it must. That restraint is the general rule for constant folding — fold only operations that cannot fail, and leave anything that can fault for the evaluator with full runtime context.

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
// Returning nil prunes the subtree; returning a Visitor continues traversal.
// This follows go/ast.Walk's pre-order convention; it omits the v.Visit(nil)
// post-visit call that go/ast.Walk makes after a node's children.
type Visitor interface {
	Visit(node Node) Visitor
}

// Walk traverses the AST rooted at node in depth-first pre-order.
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
func Inspect(node Node, fn func(Node) bool) {
	Walk(inspector(fn), node)
}

// Collect returns all nodes in the subtree rooted at root whose concrete type
// satisfies T, in depth-first pre-order.
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

Create `transform.go`:

```go
// transform.go
package ast

import "fmt"

// Clone returns a completely independent deep copy of node. Mutations to the
// returned tree do not affect the original. Clone panics on node types not
// listed here — extend the switch when you add new node types.
func Clone(node Node) Node {
	switch n := node.(type) {
	case *Program:
		c := &Program{Statements: make([]Statement, len(n.Statements))}
		for i, s := range n.Statements {
			c.Statements[i] = Clone(s).(Statement)
		}
		return c
	case *LetStatement:
		c := &LetStatement{Token: n.Token, pos: n.pos}
		c.Name = Clone(n.Name).(*Identifier)
		if n.Value != nil {
			c.Value = Clone(n.Value).(Expression)
		}
		return c
	case *ReturnStatement:
		c := &ReturnStatement{Token: n.Token, pos: n.pos}
		if n.ReturnValue != nil {
			c.ReturnValue = Clone(n.ReturnValue).(Expression)
		}
		return c
	case *ExpressionStatement:
		c := &ExpressionStatement{Token: n.Token, pos: n.pos}
		if n.Expression != nil {
			c.Expression = Clone(n.Expression).(Expression)
		}
		return c
	case *BlockStatement:
		c := &BlockStatement{Token: n.Token, pos: n.pos, Statements: make([]Statement, len(n.Statements))}
		for i, s := range n.Statements {
			c.Statements[i] = Clone(s).(Statement)
		}
		return c
	case *IfExpression:
		c := &IfExpression{Token: n.Token, pos: n.pos}
		c.Condition = Clone(n.Condition).(Expression)
		c.Consequence = Clone(n.Consequence).(*BlockStatement)
		if n.Alternative != nil {
			c.Alternative = Clone(n.Alternative).(*BlockStatement)
		}
		return c
	case *PrefixExpression:
		return &PrefixExpression{
			Token:    n.Token,
			Operator: n.Operator,
			Right:    Clone(n.Right).(Expression),
			pos:      n.pos,
		}
	case *InfixExpression:
		return &InfixExpression{
			Token:    n.Token,
			Left:     Clone(n.Left).(Expression),
			Operator: n.Operator,
			Right:    Clone(n.Right).(Expression),
			pos:      n.pos,
		}
	case *FunctionLiteral:
		c := &FunctionLiteral{Token: n.Token, Name: n.Name, pos: n.pos}
		c.Parameters = make([]*Identifier, len(n.Parameters))
		for i, p := range n.Parameters {
			c.Parameters[i] = Clone(p).(*Identifier)
		}
		c.Body = Clone(n.Body).(*BlockStatement)
		return c
	case *CallExpression:
		c := &CallExpression{Token: n.Token, pos: n.pos}
		c.Function = Clone(n.Function).(Expression)
		c.Arguments = make([]Expression, len(n.Arguments))
		for i, a := range n.Arguments {
			c.Arguments[i] = Clone(a).(Expression)
		}
		return c
	case *Identifier:
		return &Identifier{Token: n.Token, Value: n.Value, pos: n.pos}
	case *IntegerLiteral:
		return &IntegerLiteral{Token: n.Token, Value: n.Value, pos: n.pos}
	case *BooleanLiteral:
		return &BooleanLiteral{Token: n.Token, Value: n.Value, pos: n.pos}
	case *StringLiteral:
		return &StringLiteral{Token: n.Token, Value: n.Value, pos: n.pos}
	default:
		panic(fmt.Sprintf("Clone: unknown node type %T", node))
	}
}

// Equal reports whether a and b are structurally equivalent, ignoring source
// positions. Nodes are equal when they have the same type, the same semantic
// field values, and structurally equal children.
func Equal(a, b Node) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch na := a.(type) {
	case *Program:
		nb, ok := b.(*Program)
		if !ok || len(na.Statements) != len(nb.Statements) {
			return false
		}
		for i := range na.Statements {
			if !Equal(na.Statements[i], nb.Statements[i]) {
				return false
			}
		}
		return true
	case *LetStatement:
		nb, ok := b.(*LetStatement)
		return ok && Equal(na.Name, nb.Name) && Equal(na.Value, nb.Value)
	case *ReturnStatement:
		nb, ok := b.(*ReturnStatement)
		return ok && Equal(na.ReturnValue, nb.ReturnValue)
	case *ExpressionStatement:
		nb, ok := b.(*ExpressionStatement)
		return ok && Equal(na.Expression, nb.Expression)
	case *BlockStatement:
		nb, ok := b.(*BlockStatement)
		if !ok || len(na.Statements) != len(nb.Statements) {
			return false
		}
		for i := range na.Statements {
			if !Equal(na.Statements[i], nb.Statements[i]) {
				return false
			}
		}
		return true
	case *IfExpression:
		nb, ok := b.(*IfExpression)
		return ok && Equal(na.Condition, nb.Condition) &&
			Equal(na.Consequence, nb.Consequence) &&
			Equal(na.Alternative, nb.Alternative)
	case *PrefixExpression:
		nb, ok := b.(*PrefixExpression)
		return ok && na.Operator == nb.Operator && Equal(na.Right, nb.Right)
	case *InfixExpression:
		nb, ok := b.(*InfixExpression)
		return ok && na.Operator == nb.Operator &&
			Equal(na.Left, nb.Left) && Equal(na.Right, nb.Right)
	case *FunctionLiteral:
		nb, ok := b.(*FunctionLiteral)
		if !ok || len(na.Parameters) != len(nb.Parameters) {
			return false
		}
		for i := range na.Parameters {
			if !Equal(na.Parameters[i], nb.Parameters[i]) {
				return false
			}
		}
		return Equal(na.Body, nb.Body)
	case *CallExpression:
		nb, ok := b.(*CallExpression)
		if !ok || !Equal(na.Function, nb.Function) || len(na.Arguments) != len(nb.Arguments) {
			return false
		}
		for i := range na.Arguments {
			if !Equal(na.Arguments[i], nb.Arguments[i]) {
				return false
			}
		}
		return true
	case *Identifier:
		nb, ok := b.(*Identifier)
		return ok && na.Value == nb.Value
	case *IntegerLiteral:
		nb, ok := b.(*IntegerLiteral)
		return ok && na.Value == nb.Value
	case *BooleanLiteral:
		nb, ok := b.(*BooleanLiteral)
		return ok && na.Value == nb.Value
	case *StringLiteral:
		nb, ok := b.(*StringLiteral)
		return ok && na.Value == nb.Value
	default:
		return false
	}
}

// Transform traverses node bottom-up, applying fn to each node after its
// children have been transformed. If fn returns the same node, no replacement
// occurs; if it returns a new node, the caller's slot is updated. Transform
// mutates the tree in place — call Clone first to preserve the original.
func Transform(node Node, fn func(Node) Node) Node {
	switch n := node.(type) {
	case *Program:
		for i, s := range n.Statements {
			n.Statements[i] = Transform(s, fn).(Statement)
		}
	case *LetStatement:
		if n.Value != nil {
			n.Value = Transform(n.Value, fn).(Expression)
		}
	case *ReturnStatement:
		if n.ReturnValue != nil {
			n.ReturnValue = Transform(n.ReturnValue, fn).(Expression)
		}
	case *ExpressionStatement:
		if n.Expression != nil {
			n.Expression = Transform(n.Expression, fn).(Expression)
		}
	case *BlockStatement:
		for i, s := range n.Statements {
			n.Statements[i] = Transform(s, fn).(Statement)
		}
	case *IfExpression:
		n.Condition = Transform(n.Condition, fn).(Expression)
		n.Consequence = Transform(n.Consequence, fn).(*BlockStatement)
		if n.Alternative != nil {
			n.Alternative = Transform(n.Alternative, fn).(*BlockStatement)
		}
	case *PrefixExpression:
		n.Right = Transform(n.Right, fn).(Expression)
	case *InfixExpression:
		n.Left = Transform(n.Left, fn).(Expression)
		n.Right = Transform(n.Right, fn).(Expression)
	case *FunctionLiteral:
		for i, p := range n.Parameters {
			n.Parameters[i] = Transform(p, fn).(*Identifier)
		}
		n.Body = Transform(n.Body, fn).(*BlockStatement)
	case *CallExpression:
		n.Function = Transform(n.Function, fn).(Expression)
		for i, a := range n.Arguments {
			n.Arguments[i] = Transform(a, fn).(Expression)
		}
	// Leaf nodes have no children to transform.
	case *Identifier, *IntegerLiteral, *BooleanLiteral, *StringLiteral:
	}
	return fn(node)
}

// ConstantFold returns a Transform-compatible function that folds constant
// integer arithmetic and boolean operations at compile time. Only operations
// that cannot fail at runtime are folded: division by zero is left intact.
// Multi-level folding works because Transform is bottom-up: by the time the
// outer node is visited, its children are already folded literals.
func ConstantFold() func(Node) Node {
	return func(n Node) Node {
		infix, ok := n.(*InfixExpression)
		if !ok {
			return n
		}
		// Integer arithmetic: both operands must be integer literals.
		li, liOK := infix.Left.(*IntegerLiteral)
		ri, riOK := infix.Right.(*IntegerLiteral)
		if liOK && riOK {
			var result int64
			switch infix.Operator {
			case "+":
				result = li.Value + ri.Value
			case "-":
				result = li.Value - ri.Value
			case "*":
				result = li.Value * ri.Value
			case "/":
				if ri.Value == 0 {
					return n // do not fold; runtime must handle the error
				}
				result = li.Value / ri.Value
			default:
				return n
			}
			return &IntegerLiteral{
				Token: fmt.Sprintf("%d", result),
				Value: result,
				pos:   infix.pos,
			}
		}
		// Boolean operations.
		lb, lbOK := infix.Left.(*BooleanLiteral)
		rb, rbOK := infix.Right.(*BooleanLiteral)
		if lbOK && rbOK {
			var result bool
			switch infix.Operator {
			case "&&":
				result = lb.Value && rb.Value
			case "||":
				result = lb.Value || rb.Value
			default:
				return n
			}
			tok := "false"
			if result {
				tok = "true"
			}
			return &BooleanLiteral{Token: tok, Value: result, pos: infix.pos}
		}
		return n
	}
}
```

### The runnable demo

The demo builds `let result = (2 + 3) * 4;`, clones it, folds the clone, and prints both the count of integer literals before and after (three operands become one folded literal) and confirms with `Equal` that the original was preserved. The clone-then-transform pairing is the whole point: the original still reports three literals after the fold.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/transform-clone-equal"
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

	fmt.Println("Original AST:")
	fmt.Println(prog.String())

	before := ast.Collect[*ast.IntegerLiteral](prog)
	fmt.Printf("Integer literals before fold: %d\n", len(before))

	// Clone before transforming so the original is preserved.
	folded := ast.Transform(ast.Clone(prog), ast.ConstantFold()).(*ast.Program)
	fmt.Println("\nAfter constant folding:")
	fmt.Println(folded.String())

	after := ast.Collect[*ast.IntegerLiteral](folded)
	fmt.Printf("Integer literals after fold: %d\n", len(after))

	fmt.Printf("\nOriginals are structurally equal to themselves: %v\n",
		ast.Equal(prog, prog))
	fmt.Printf("Original and folded are NOT equal: %v\n",
		!ast.Equal(prog, folded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
Original AST:
let result = ((2 + 3) * 4);
Integer literals before fold: 3

After constant folding:
let result = 20;
Integer literals after fold: 1

Originals are structurally equal to themselves: true
Original and folded are NOT equal: true
```

### Tests

The tests pin each primitive: clone independence (mutate the clone, the original is unchanged), position-blind equality (same value, different positions compare equal; different values do not), the four folding behaviors (integer arithmetic, division-by-zero left intact, nested single-pass collapse, boolean fold), and the clone-then-transform invariant that the original keeps both of its literals.

Create `transform_test.go`:

```go
// transform_test.go
package ast

import (
	"fmt"
	"testing"
)

func TestCloneIsIndependent(t *testing.T) {
	t.Parallel()
	orig := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: NewIntegerLiteral("1", 1, Position{}),
			},
		},
	}
	cloned := Clone(orig).(*Program)
	// Mutate the clone.
	cloned.Statements[0].(*LetStatement).Name.Value = "z"
	// Original must be unchanged.
	got := orig.Statements[0].(*LetStatement).Name.Value
	if got != "x" {
		t.Fatalf("original mutated: Name.Value = %q, want %q", got, "x")
	}
}

func TestEqualIgnoresPositions(t *testing.T) {
	t.Parallel()
	a := NewIntegerLiteral("5", 5, Position{Line: 1, Column: 1})
	b := NewIntegerLiteral("5", 5, Position{Line: 99, Column: 42})
	if !Equal(a, b) {
		t.Fatal("Equal returned false for structurally identical nodes with different positions")
	}
}

func TestEqualDistinguishesDifferentValues(t *testing.T) {
	t.Parallel()
	a := NewIntegerLiteral("5", 5, Position{})
	b := NewIntegerLiteral("6", 6, Position{})
	if Equal(a, b) {
		t.Fatal("Equal returned true for integer literals with different values")
	}
}

func TestConstantFoldIntegerArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		left, right int64
		op          string
		want        int64
	}{
		{3, 4, "+", 7},
		{10, 3, "-", 7},
		{3, 4, "*", 12},
		{10, 2, "/", 5},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d%s%d", tc.left, tc.op, tc.right), func(t *testing.T) {
			t.Parallel()
			node := &InfixExpression{
				Token:    tc.op,
				Left:     NewIntegerLiteral(fmt.Sprintf("%d", tc.left), tc.left, Position{}),
				Operator: tc.op,
				Right:    NewIntegerLiteral(fmt.Sprintf("%d", tc.right), tc.right, Position{}),
			}
			result := Transform(node, ConstantFold())
			lit, ok := result.(*IntegerLiteral)
			if !ok {
				t.Fatalf("expected *IntegerLiteral after fold, got %T", result)
			}
			if lit.Value != tc.want {
				t.Fatalf("fold(%d %s %d) = %d, want %d", tc.left, tc.op, tc.right, lit.Value, tc.want)
			}
		})
	}
}

func TestConstantFoldDivisionByZeroUnchanged(t *testing.T) {
	t.Parallel()
	node := &InfixExpression{
		Token:    "/",
		Left:     NewIntegerLiteral("5", 5, Position{}),
		Operator: "/",
		Right:    NewIntegerLiteral("0", 0, Position{}),
	}
	result := Transform(node, ConstantFold())
	if _, ok := result.(*IntegerLiteral); ok {
		t.Fatal("division by zero must not be constant-folded")
	}
}

func TestConstantFoldNestedExpression(t *testing.T) {
	t.Parallel()
	// (2 + 3) * 4  =>  5 * 4  =>  20
	node := &InfixExpression{
		Token: "*",
		Left: &InfixExpression{
			Token:    "+",
			Left:     NewIntegerLiteral("2", 2, Position{}),
			Operator: "+",
			Right:    NewIntegerLiteral("3", 3, Position{}),
		},
		Operator: "*",
		Right:    NewIntegerLiteral("4", 4, Position{}),
	}
	result := Transform(node, ConstantFold())
	lit, ok := result.(*IntegerLiteral)
	if !ok {
		t.Fatalf("expected *IntegerLiteral after multi-level fold, got %T", result)
	}
	if lit.Value != 20 {
		t.Fatalf("fold result = %d, want 20", lit.Value)
	}
}

func TestConstantFoldBooleanAnd(t *testing.T) {
	t.Parallel()
	node := &InfixExpression{
		Token:    "&&",
		Left:     NewBooleanLiteral("true", true, Position{}),
		Operator: "&&",
		Right:    NewBooleanLiteral("false", false, Position{}),
	}
	result := Transform(node, ConstantFold())
	lit, ok := result.(*BooleanLiteral)
	if !ok {
		t.Fatalf("expected *BooleanLiteral after fold, got %T", result)
	}
	if lit.Value {
		t.Fatal("true && false should fold to false")
	}
}

func TestTransformPreservesOriginalWhenCloned(t *testing.T) {
	t.Parallel()
	orig := &Program{
		Statements: []Statement{
			&LetStatement{
				Token: "let",
				Name:  NewIdentifier("x", "x", Position{}),
				Value: &InfixExpression{
					Token:    "+",
					Left:     NewIntegerLiteral("2", 2, Position{}),
					Operator: "+",
					Right:    NewIntegerLiteral("3", 3, Position{}),
				},
			},
		},
	}
	folded := Transform(Clone(orig), ConstantFold()).(*Program)
	// Folded tree has one IntegerLiteral (value 5); original still has two.
	if len(Collect[*IntegerLiteral](folded)) != 1 {
		t.Fatal("folded tree should contain exactly one integer literal")
	}
	if len(Collect[*IntegerLiteral](orig)) != 2 {
		t.Fatal("original tree must not be mutated by Transform on the clone")
	}
}

// ExampleTransform_constantFold demonstrates multi-level constant folding:
// (2 + 3) * 4 is reduced to 20 in a single bottom-up pass.
func ExampleTransform_constantFold() {
	node := &InfixExpression{
		Token: "*",
		Left: &InfixExpression{
			Token:    "+",
			Left:     NewIntegerLiteral("2", 2, Position{}),
			Operator: "+",
			Right:    NewIntegerLiteral("3", 3, Position{}),
		},
		Operator: "*",
		Right:    NewIntegerLiteral("4", 4, Position{}),
	}
	result := Transform(node, ConstantFold())
	fmt.Println(result.String())
	// Output:
	// 20
}
```

## Review

The primitives are correct when copies are independent, equality is blind to position but sensitive to value, and folding collapses constants in a single bottom-up pass. Confirm that mutating a clone leaves the source untouched, that two `5` literals with different positions are `Equal` while a `5` and a `6` are not, that integer and boolean folds produce the right literal, and that `(2 + 3) * 4` folds to a single `20` while a divide-by-zero is returned un-folded for the runtime. The clone-then-transform invariant is the capstone: after `Transform(Clone(prog), ConstantFold())`, the folded tree holds one integer literal and the original still holds two.

Two pitfalls dominate. Reaching for `reflect.DeepEqual` instead of `Equal` makes structurally identical trees compare unequal whenever positions differ — after a clone or a re-parse — which is exactly the case that matters; the hand-written `Equal` skips `pos` on purpose. And calling `Transform(prog, ...)` without cloning mutates `prog` in place, so any pointer saved into the tree before the pass may now point at a replaced node; the fix is the canonical `Transform(Clone(prog), ...)` pairing.

## Resources

- [go/ast package](https://pkg.go.dev/go/ast) — the standard library's AST, whose nodes these manipulation primitives parallel.
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — the general comparator `Equal` deliberately replaces so positions can be ignored.
- [Writing An Interpreter In Go](https://interpreterbook.com/) — the Monkey language whose tree these passes operate on.
- [Crafting Interpreters: Representing Code](https://craftinginterpreters.com/representing-code.html) — AST design and the tree-rewriting passes a compiler runs over it.

---

Back to [02-visitor-traversal.md](02-visitor-traversal.md) | Next: [../04-tree-walking-evaluator/00-concepts.md](../04-tree-walking-evaluator/00-concepts.md)
