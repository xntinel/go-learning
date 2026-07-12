# Exercise 2: Loops and Control-Flow Signals

The core evaluator handles `return` with a single wrapper type relayed up to the call boundary. Loops generalize that idea: `while` needs two more signals, `break` and `continue`, and they must travel out through any nesting of `if` blocks to the loop that owns them — no further. This module builds that signal-propagation machinery in isolation: a `BreakSignal` and a `ContinueSignal` that `evalBlockStatement` relays without consuming, and an `evalWhileExpression` that is the one place either signal is finally consumed. Getting the relay-versus-consume split right is the whole exercise.

This module is fully self-contained. It depends on nothing but the standard library, reproduces the small AST it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ast.go            AST nodes: while, break, continue, if, let, infix, literals
object.go         Integer, Boolean, Null, Error, BreakSignal, ContinueSignal
env.go            Environment: the scope chain
eval.go           Eval, evalBlockStatement, evalWhileExpression
eval_test.go      while-sum, break, continue, error propagation
cmd/
  demo/
    main.go       run a sum loop, a break loop, and a continue loop
```

- Files: `ast.go`, `object.go`, `env.go`, `eval.go`, `cmd/demo/main.go`, `eval_test.go`.
- Implement: `BreakSignal`/`ContinueSignal`, an `evalBlockStatement` that relays both signals plus errors, and `evalWhileExpression` that consumes `break` (exit) and `continue` (next iteration) while propagating errors.
- Test: a counting `while` sum, `break` exits early, `continue` skips the rest of the body, and a runtime error inside the body propagates out of the loop.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/04-tree-walking-evaluator/02-loops-and-control-flow/cmd/demo && cd go-solutions/40-capstone-language-interpreter/04-tree-walking-evaluator/02-loops-and-control-flow
```

### Why break and continue are signals, not jumps

A tree-walking evaluator has no program counter to jump with, so loop control is expressed the same way as `return`: a sentinel value that propagates up the call stack until something consumes it. `break` and `continue` each become a package-level singleton object — `BREAK` and `CONTINUE` — and `Eval` of a `*BreakStatement` simply returns `BREAK`. The signal then has to climb out of however many `if` blocks surround it to reach the loop. That climb is `evalBlockStatement`'s job.

### The relay-versus-consume split

`evalBlockStatement` evaluates a block's statements and, the instant one produces a `BreakSignal`, a `ContinueSignal`, or an `Error`, returns it immediately **without acting on it**. It is a relay. This is what lets `break` sit inside `if (i == 3) { break }` inside a `while` body and still reach the loop: the `if` block returns the `BreakSignal` upward, the surrounding body block relays it again, and only then does the loop see it. If `evalBlockStatement` tried to interpret the signal itself it would consume it at the wrong depth and the loop would never exit.

`evalWhileExpression` is the single consumer. After each pass over the body it inspects the result: `BreakType` means stop and return (the loop is done), `ContinueType` means discard and start the next iteration, `ErrorType` means propagate the error out of the loop entirely. Anything else (a normal value or `NULL`) just falls through to the next condition check. Because `break` is consumed here and nowhere else, a `break` outside any loop would propagate harmlessly to the top instead of corrupting an unrelated block — the signal only means something where it is consumed.

### The loop body shares the loop's environment

`evalWhileExpression` evaluates the body in the same environment as the loop, not a fresh scope per iteration. That is deliberate: a counter updated with `let i = i + 1` inside the body must be visible to the condition on the next pass, and to the code after the loop. `Set` overwrites the existing binding in place, so re-binding `i` each iteration mutates the one `i` the loop reads. A new scope per iteration would hide each update and the loop would never make progress.

Create `ast.go`:

```go
package loops

// Node is the base interface every AST element implements.
type Node interface{ nodeTag() }

// Statement nodes produce effects.
type Statement interface {
	Node
	stmtTag()
}

// Expression nodes produce values.
type Expression interface {
	Node
	exprTag()
}

// Program is the root node.
type Program struct{ Statements []Statement }

func (p *Program) nodeTag() {}

// BlockStatement holds the statements between a pair of braces.
type BlockStatement struct{ Statements []Statement }

func (b *BlockStatement) nodeTag() {}
func (b *BlockStatement) stmtTag() {}

// LetStatement binds (or re-binds) Name to the result of Value.
type LetStatement struct {
	Name  string
	Value Expression
}

func (l *LetStatement) nodeTag() {}
func (l *LetStatement) stmtTag() {}

// ExpressionStatement wraps an expression used in statement position.
type ExpressionStatement struct{ Expr Expression }

func (e *ExpressionStatement) nodeTag() {}
func (e *ExpressionStatement) stmtTag() {}

// BreakStatement exits the nearest enclosing loop.
type BreakStatement struct{}

func (b *BreakStatement) nodeTag() {}
func (b *BreakStatement) stmtTag() {}

// ContinueStatement skips to the next loop iteration.
type ContinueStatement struct{}

func (c *ContinueStatement) nodeTag() {}
func (c *ContinueStatement) stmtTag() {}

// IntegerLiteral holds a parsed int64.
type IntegerLiteral struct{ Value int64 }

func (i *IntegerLiteral) nodeTag() {}
func (i *IntegerLiteral) exprTag() {}

// BooleanLiteral holds true or false.
type BooleanLiteral struct{ Value bool }

func (b *BooleanLiteral) nodeTag() {}
func (b *BooleanLiteral) exprTag() {}

// Identifier is a variable reference.
type Identifier struct{ Name string }

func (i *Identifier) nodeTag() {}
func (i *Identifier) exprTag() {}

// InfixExpression applies Operator between Left and Right.
type InfixExpression struct {
	Left     Expression
	Operator string
	Right    Expression
}

func (i *InfixExpression) nodeTag() {}
func (i *InfixExpression) exprTag() {}

// IfExpression evaluates Condition and branches on truthiness. Alternative is
// nil when there is no else branch.
type IfExpression struct {
	Condition   Expression
	Consequence *BlockStatement
	Alternative *BlockStatement
}

func (i *IfExpression) nodeTag() {}
func (i *IfExpression) exprTag() {}

// WhileExpression evaluates Body repeatedly while Condition is truthy.
type WhileExpression struct {
	Condition Expression
	Body      *BlockStatement
}

func (w *WhileExpression) nodeTag() {}
func (w *WhileExpression) exprTag() {}
```

Create `object.go`:

```go
package loops

import "fmt"

// ObjectType is the string tag identifying a runtime value's kind.
type ObjectType string

const (
	IntegerType  ObjectType = "INTEGER"
	BooleanType  ObjectType = "BOOLEAN"
	NullType     ObjectType = "NULL"
	ErrorType    ObjectType = "ERROR"
	BreakType    ObjectType = "BREAK"
	ContinueType ObjectType = "CONTINUE"
)

// Object is implemented by every runtime value.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer wraps int64.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return IntegerType }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// Boolean wraps bool.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BooleanType }
func (b *Boolean) Inspect() string  { return fmt.Sprintf("%t", b.Value) }

// Null is the singleton null value.
type Null struct{}

func (n *Null) Type() ObjectType { return NullType }
func (n *Null) Inspect() string  { return "null" }

// Error represents a runtime error. It propagates out of loops without being
// consumed by the loop handler.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ErrorType }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// BreakSignal propagates a break statement out to the nearest loop. It is a
// singleton (BREAK) and carries no value.
type BreakSignal struct{}

func (b *BreakSignal) Type() ObjectType { return BreakType }
func (b *BreakSignal) Inspect() string  { return "break" }

// ContinueSignal propagates a continue to the next loop iteration. It is a
// singleton (CONTINUE) and carries no value.
type ContinueSignal struct{}

func (c *ContinueSignal) Type() ObjectType { return ContinueType }
func (c *ContinueSignal) Inspect() string  { return "continue" }
```

Create `env.go`:

```go
package loops

// Environment maps variable names to Objects and forms a chain of scopes. The
// while loop reuses one environment across iterations so a counter updated in
// the body is visible to the next condition check.
type Environment struct {
	store map[string]Object
}

// NewEnvironment returns a fresh top-level environment.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// Get looks up name in the environment.
func (e *Environment) Get(name string) (Object, bool) {
	obj, ok := e.store[name]
	return obj, ok
}

// Set creates or overwrites name. Re-binding a loop counter with let relies on
// this overwrite-in-place behavior.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}
```

Create `eval.go`:

```go
package loops

import "fmt"

// Singletons. BREAK and CONTINUE carry no value, so one shared instance of each
// is enough and lets handlers compare by type.
var (
	TRUE     = &Boolean{Value: true}
	FALSE    = &Boolean{Value: false}
	NULL     = &Null{}
	BREAK    = &BreakSignal{}
	CONTINUE = &ContinueSignal{}
)

func nativeBool(b bool) *Boolean {
	if b {
		return TRUE
	}
	return FALSE
}

func isError(obj Object) bool {
	return obj != nil && obj.Type() == ErrorType
}

// isTruthy implements Monkey truthiness: only false and null are falsy.
func isTruthy(obj Object) bool {
	switch o := obj.(type) {
	case *Null:
		return false
	case *Boolean:
		return o.Value
	default:
		return true
	}
}

func newError(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// Eval recursively evaluates node in env and never panics.
func Eval(node Node, env *Environment) Object {
	switch n := node.(type) {
	case *Program:
		return evalProgram(n, env)
	case *BlockStatement:
		return evalBlockStatement(n, env)
	case *ExpressionStatement:
		return Eval(n.Expr, env)
	case *LetStatement:
		val := Eval(n.Value, env)
		if isError(val) {
			return val
		}
		env.Set(n.Name, val)
		return NULL
	case *BreakStatement:
		return BREAK
	case *ContinueStatement:
		return CONTINUE
	case *IntegerLiteral:
		return &Integer{Value: n.Value}
	case *BooleanLiteral:
		return nativeBool(n.Value)
	case *Identifier:
		return evalIdentifier(n, env)
	case *InfixExpression:
		left := Eval(n.Left, env)
		if isError(left) {
			return left
		}
		right := Eval(n.Right, env)
		if isError(right) {
			return right
		}
		return evalInfixExpression(n.Operator, left, right)
	case *IfExpression:
		return evalIfExpression(n, env)
	case *WhileExpression:
		return evalWhileExpression(n, env)
	}
	return newError("unknown node type: %T", node)
}

func evalProgram(prog *Program, env *Environment) Object {
	var result Object
	for _, stmt := range prog.Statements {
		result = Eval(stmt, env)
		if isError(result) {
			return result
		}
	}
	return result
}

// evalBlockStatement relays Break, Continue, and Error upward without consuming
// them. This is what lets a break inside a nested if reach the enclosing loop.
func evalBlockStatement(block *BlockStatement, env *Environment) Object {
	var result Object
	for _, stmt := range block.Statements {
		result = Eval(stmt, env)
		if result != nil {
			switch result.Type() {
			case ErrorType, BreakType, ContinueType:
				return result // propagate without consuming
			}
		}
	}
	return result
}

func evalIdentifier(node *Identifier, env *Environment) Object {
	if val, ok := env.Get(node.Name); ok {
		return val
	}
	return newError("identifier not found: %s", node.Name)
}

func evalInfixExpression(op string, left, right Object) Object {
	l, lok := left.(*Integer)
	r, rok := right.(*Integer)
	if !lok || !rok {
		return newError("unknown operator: %s %s %s", left.Type(), op, right.Type())
	}
	switch op {
	case "+":
		return &Integer{Value: l.Value + r.Value}
	case "-":
		return &Integer{Value: l.Value - r.Value}
	case "*":
		return &Integer{Value: l.Value * r.Value}
	case "/":
		if r.Value == 0 {
			return newError("division by zero")
		}
		return &Integer{Value: l.Value / r.Value}
	case "%":
		if r.Value == 0 {
			return newError("modulo by zero")
		}
		return &Integer{Value: l.Value % r.Value}
	case "<":
		return nativeBool(l.Value < r.Value)
	case ">":
		return nativeBool(l.Value > r.Value)
	case "<=":
		return nativeBool(l.Value <= r.Value)
	case ">=":
		return nativeBool(l.Value >= r.Value)
	case "==":
		return nativeBool(l.Value == r.Value)
	case "!=":
		return nativeBool(l.Value != r.Value)
	}
	return newError("unknown operator: INTEGER %s INTEGER", op)
}

func evalIfExpression(node *IfExpression, env *Environment) Object {
	cond := Eval(node.Condition, env)
	if isError(cond) {
		return cond
	}
	if isTruthy(cond) {
		return Eval(node.Consequence, env)
	}
	if node.Alternative != nil {
		return Eval(node.Alternative, env)
	}
	return NULL
}

// evalWhileExpression is the single consumer of Break and Continue. It evaluates
// the body in the loop's own environment (no new scope per iteration) and
// propagates errors out of the loop.
func evalWhileExpression(node *WhileExpression, env *Environment) Object {
	var result Object = NULL
	for {
		cond := Eval(node.Condition, env)
		if isError(cond) {
			return cond
		}
		if !isTruthy(cond) {
			break
		}
		result = evalBlockStatement(node.Body, env)
		if result == nil {
			continue
		}
		switch result.Type() {
		case ErrorType:
			return result // propagate out of the loop
		case BreakType:
			return NULL // consume break, exit the loop
		case ContinueType:
			continue // consume continue, next iteration
		}
	}
	return result
}
```

### The runnable demo

The demo runs three loops: a counting sum (`1..5 == 15`), a `break` loop that stops the counter at 3, and a `continue` loop that adds only the odd values of `1..5` (`1 + 3 + 5 == 9`). Small local constructors keep the hand-built ASTs readable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/loops"
)

func intLit(v int64) *loops.IntegerLiteral { return &loops.IntegerLiteral{Value: v} }
func ident(n string) *loops.Identifier     { return &loops.Identifier{Name: n} }

func infix(l loops.Expression, op string, r loops.Expression) *loops.InfixExpression {
	return &loops.InfixExpression{Left: l, Operator: op, Right: r}
}

func exprStmt(e loops.Expression) *loops.ExpressionStatement {
	return &loops.ExpressionStatement{Expr: e}
}

func letStmt(name string, v loops.Expression) *loops.LetStatement {
	return &loops.LetStatement{Name: name, Value: v}
}

func blk(stmts ...loops.Statement) *loops.BlockStatement {
	return &loops.BlockStatement{Statements: stmts}
}

func run(stmts ...loops.Statement) string {
	env := loops.NewEnvironment()
	return loops.Eval(&loops.Program{Statements: stmts}, env).Inspect()
}

func main() {
	// 1. Sum 1..5 with a while loop.
	sum := run(
		letStmt("i", intLit(0)),
		letStmt("sum", intLit(0)),
		exprStmt(&loops.WhileExpression{
			Condition: infix(ident("i"), "<", intLit(5)),
			Body: blk(
				letStmt("i", infix(ident("i"), "+", intLit(1))),
				letStmt("sum", infix(ident("sum"), "+", ident("i"))),
			),
		}),
		exprStmt(ident("sum")),
	)
	fmt.Println("sum 1..5:", sum)

	// 2. Break: count up but stop at 3.
	brk := run(
		letStmt("i", intLit(0)),
		exprStmt(&loops.WhileExpression{
			Condition: &loops.BooleanLiteral{Value: true},
			Body: blk(
				exprStmt(&loops.IfExpression{
					Condition:   infix(ident("i"), "==", intLit(3)),
					Consequence: blk(&loops.BreakStatement{}),
				}),
				letStmt("i", infix(ident("i"), "+", intLit(1))),
			),
		}),
		exprStmt(ident("i")),
	)
	fmt.Println("break at i==3:", brk)

	// 3. Continue: sum only the odd values of 1..5.
	odd := run(
		letStmt("i", intLit(0)),
		letStmt("sum", intLit(0)),
		exprStmt(&loops.WhileExpression{
			Condition: infix(ident("i"), "<", intLit(5)),
			Body: blk(
				letStmt("i", infix(ident("i"), "+", intLit(1))),
				exprStmt(&loops.IfExpression{
					Condition:   infix(infix(ident("i"), "%", intLit(2)), "==", intLit(0)),
					Consequence: blk(&loops.ContinueStatement{}),
				}),
				letStmt("sum", infix(ident("sum"), "+", ident("i"))),
			),
		}),
		exprStmt(ident("sum")),
	)
	fmt.Println("continue odd-sum 1..5:", odd)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sum 1..5: 15
break at i==3: 3
continue odd-sum 1..5: 9
```

### Tests

The tests pin each signal's effect. `TestWhileBreak` proves `break` exits before the increment runs, so the counter freezes at 3. `TestWhileContinue` proves `continue` skips the rest of the body, leaving only the odd-indexed additions. `TestErrorInLoop` proves a division-by-zero inside the body propagates out as an `*Error` instead of being swallowed by the loop.

Create `eval_test.go`:

```go
package loops

import (
	"fmt"
	"testing"
)

func intLit(v int64) *IntegerLiteral { return &IntegerLiteral{Value: v} }
func ident(n string) *Identifier     { return &Identifier{Name: n} }

func infix(l Expression, op string, r Expression) *InfixExpression {
	return &InfixExpression{Left: l, Operator: op, Right: r}
}

func exprStmt(e Expression) *ExpressionStatement {
	return &ExpressionStatement{Expr: e}
}

func letStmt(name string, v Expression) *LetStatement {
	return &LetStatement{Name: name, Value: v}
}

func blk(stmts ...Statement) *BlockStatement {
	return &BlockStatement{Statements: stmts}
}

func evalInt(t *testing.T, stmts ...Statement) int64 {
	t.Helper()
	got := Eval(&Program{Statements: stmts}, NewEnvironment())
	i, ok := got.(*Integer)
	if !ok {
		t.Fatalf("Eval = %T(%v), want *Integer", got, got)
	}
	return i.Value
}

func TestWhileSum(t *testing.T) {
	t.Parallel()

	got := evalInt(t,
		letStmt("i", intLit(0)),
		letStmt("sum", intLit(0)),
		exprStmt(&WhileExpression{
			Condition: infix(ident("i"), "<", intLit(5)),
			Body: blk(
				letStmt("i", infix(ident("i"), "+", intLit(1))),
				letStmt("sum", infix(ident("sum"), "+", ident("i"))),
			),
		}),
		exprStmt(ident("sum")),
	)
	if got != 15 {
		t.Errorf("sum 1..5 = %d, want 15", got)
	}
}

// TestWhileBreak verifies break exits the loop before the increment, freezing
// the counter at 3.
func TestWhileBreak(t *testing.T) {
	t.Parallel()

	got := evalInt(t,
		letStmt("i", intLit(0)),
		exprStmt(&WhileExpression{
			Condition: &BooleanLiteral{Value: true},
			Body: blk(
				exprStmt(&IfExpression{
					Condition:   infix(ident("i"), "==", intLit(3)),
					Consequence: blk(&BreakStatement{}),
				}),
				letStmt("i", infix(ident("i"), "+", intLit(1))),
			),
		}),
		exprStmt(ident("i")),
	)
	if got != 3 {
		t.Errorf("i after break = %d, want 3", got)
	}
}

// TestWhileContinue verifies continue skips the rest of the body, summing only
// the odd values of 1..5.
func TestWhileContinue(t *testing.T) {
	t.Parallel()

	got := evalInt(t,
		letStmt("i", intLit(0)),
		letStmt("sum", intLit(0)),
		exprStmt(&WhileExpression{
			Condition: infix(ident("i"), "<", intLit(5)),
			Body: blk(
				letStmt("i", infix(ident("i"), "+", intLit(1))),
				exprStmt(&IfExpression{
					Condition:   infix(infix(ident("i"), "%", intLit(2)), "==", intLit(0)),
					Consequence: blk(&ContinueStatement{}),
				}),
				letStmt("sum", infix(ident("sum"), "+", ident("i"))),
			),
		}),
		exprStmt(ident("sum")),
	)
	if got != 9 {
		t.Errorf("odd-sum 1..5 = %d, want 9", got)
	}
}

// TestErrorInLoop verifies a runtime error inside the body propagates out of the
// loop instead of being consumed.
func TestErrorInLoop(t *testing.T) {
	t.Parallel()

	got := Eval(&Program{Statements: []Statement{
		exprStmt(&WhileExpression{
			Condition: &BooleanLiteral{Value: true},
			Body: blk(
				exprStmt(infix(intLit(1), "/", intLit(0))),
			),
		}),
	}}, NewEnvironment())
	e, ok := got.(*Error)
	if !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
	if e.Message != "division by zero" {
		t.Errorf("message = %q, want %q", e.Message, "division by zero")
	}
}

func ExampleEval_break() {
	env := NewEnvironment()
	p := &Program{Statements: []Statement{
		letStmt("i", intLit(0)),
		exprStmt(&WhileExpression{
			Condition: &BooleanLiteral{Value: true},
			Body: blk(
				exprStmt(&IfExpression{
					Condition:   infix(ident("i"), "==", intLit(3)),
					Consequence: blk(&BreakStatement{}),
				}),
				letStmt("i", infix(ident("i"), "+", intLit(1))),
			),
		}),
		exprStmt(ident("i")),
	}}
	fmt.Println(Eval(p, env).Inspect())
	// Output: 3
}
```

## Review

The machinery is correct when each signal is consumed at exactly one level. `break` must exit the loop and only the loop: `TestWhileBreak` freezes the counter at 3 because the signal escapes the inner `if` block, is relayed by the body block, and is consumed by `evalWhileExpression` before the increment runs. `continue` must skip the remainder of the body and resume: `TestWhileContinue` sums `1 + 3 + 5 == 9` because the even iterations bail out before the addition. An error must not be swallowed: `TestErrorInLoop` shows a division-by-zero climbing out of an infinite loop as an `*Error`, which also proves the loop terminates on error rather than spinning forever.

The mistakes here mirror the `return` ones. Consuming `break` or `continue` inside `evalBlockStatement` instead of relaying it means a signal nested under an `if` never reaches the loop. Giving the body a fresh scope per iteration hides counter updates and the loop never progresses. Forgetting the `ErrorType` case in `evalWhileExpression` turns a runtime error inside a `while (true)` into a hang. And handling `break` but not `continue` (or vice versa) silently drops one of the two.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — the `ReturnValue` propagation pattern this module generalizes to `break` and `continue`.
- [Crafting Interpreters: Control Flow, Robert Nystrom](https://craftinginterpreters.com/control-flow.html) — loops and conditionals in a tree-walking interpreter.
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the dispatch used to classify each signal by type.

---

Back to [01-tree-walking-evaluator.md](01-tree-walking-evaluator.md) | Next: [03-arrays-and-indexing.md](03-arrays-and-indexing.md)
