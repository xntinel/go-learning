# Exercise 1: Tree-Walking Evaluator

This is the core of the interpreter: a recursive `Eval` that turns an abstract syntax tree into runtime values. It handles arithmetic and comparison operators, string concatenation, variable binding with `let`, conditional `if`/`else`, first-class functions, and closures, and it reports every runtime failure as an `*Error` value rather than panicking. The two ideas that make it correct are the `isError` short-circuit after each child evaluation and the unwrap-at-the-boundary discipline for `ReturnValue`.

This module is fully self-contained. It depends on nothing but the standard library, reproduces the AST node types it needs in `ast.go` (in a full interpreter these come from the parser of lesson 03), and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ast.go            AST node interfaces and concrete node types
object.go         runtime value types and the Object interface
env.go            Environment: the scope chain
eval.go           Eval and its helpers
eval_test.go      table-driven tests + Example functions
cmd/
  demo/
    main.go       evaluate arithmetic, a binding, a closure, and an error
```

- Files: `ast.go`, `object.go`, `env.go`, `eval.go`, `cmd/demo/main.go`, `eval_test.go`.
- Implement: `Object`, the concrete value types, `Environment` with `Get`/`Set`/`NewEnclosedEnvironment`, and `Eval(node Node, env *Environment) Object` with its helpers (`evalProgram`, `evalBlockStatement`, `applyFunction`, the operator handlers).
- Test: arithmetic and operator precedence, division by zero, string concatenation, truthiness, comparisons, `let`/identifier, undefined variable, `if`/`else`, early `return`, closure capture, recursion, type mismatch, error propagation, arity mismatch, mixed int/float promotion.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p evaluator/cmd/demo && cd evaluator
go mod init example.com/evaluator
```

### How Eval dispatches and why errors are values

`Eval` is one function with a type switch over the node. Each case either produces a value directly (an `*IntegerLiteral` becomes an `*Integer`) or recurses into children and combines their results (an `*InfixExpression` evaluates both sides, then applies the operator). The type switch is the entire dispatch table; there is no virtual method on the AST and no visitor object. This keeps the evaluator readable as a single flat enumeration of the language.

Runtime errors are returned, never panicked. `isError` checks an object's `Type()` and the rule is mechanical: after every `Eval` of a child that might fail, check it and return early. The payoff is visible in `(1/0) + 5`. The left side evaluates to a division-by-zero error; the `isError(left)` check returns it before the right side is even evaluated, so the error is the result of the whole expression instead of being fed as a bogus operand into the infix handler. Without the check the handler would not recognize the error, fall through to "unknown operator", and bury the real cause.

### Where ReturnValue is unwrapped

`return` is a signal, not a value. `Eval` of a `*ReturnStatement` wraps the returned value in a `*ReturnValue`. From there two functions cooperate. `evalBlockStatement` evaluates the statements of a block and, the moment it sees a `*ReturnValue` or an `*Error`, returns it **without unwrapping** — it is a relay that lets the signal pass through nested blocks untouched. The unwrap happens exactly at the boundaries that own it: `evalProgram` unwraps a top-level `ReturnValue` because that ends the program, and `applyFunction` unwraps it after running the function body because that is where a function exits. This is what makes `fn() { return 10; 99 }()` evaluate to `10`: the `return` inside the body survives the block relay and is unwrapped at the call boundary, so the trailing `99` is never reached.

### Why closures capture the definition environment

When `Eval` meets a `*FunctionLiteral` it builds a `*Function` that stores a pointer to the current `env` — the environment as it exists at definition time. `applyFunction` later creates a fresh `NewEnclosedEnvironment(f.Env)`, binds the parameters there, and evaluates the body in it. Because the enclosed scope chains to `f.Env` and not to the caller's scope, the function sees the bindings that were visible where it was written. That is exactly what a closure is: `fn(x){ fn(y){ x+y } }(5)(10)` returns `15` because the inner function's captured environment still holds `x = 5` long after the outer call returned. The fresh environment per call is also what makes recursion correct — each activation of `fact(n)` gets its own `n`.

### Monkey truthiness

`isTruthy` is a three-case switch: `*Null` is false, `*Boolean` returns its field, everything else is true. Under this rule `0` and `""` are truthy and only `false` and `null` are falsy. `TestTruthiness` pins it so the contract cannot silently regress.

Create `ast.go`:

```go
package evaluator

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

// Program is the root node produced by the parser.
type Program struct{ Statements []Statement }

func (p *Program) nodeTag() {}

// BlockStatement holds the statements between a pair of braces.
type BlockStatement struct{ Statements []Statement }

func (b *BlockStatement) nodeTag() {}
func (b *BlockStatement) stmtTag() {}

// LetStatement binds Name to the result of Value.
type LetStatement struct {
	Name  string
	Value Expression
}

func (l *LetStatement) nodeTag() {}
func (l *LetStatement) stmtTag() {}

// ReturnStatement exits the enclosing function with Value.
type ReturnStatement struct{ Value Expression }

func (r *ReturnStatement) nodeTag() {}
func (r *ReturnStatement) stmtTag() {}

// ExpressionStatement wraps an expression used in statement position.
type ExpressionStatement struct{ Expr Expression }

func (e *ExpressionStatement) nodeTag() {}
func (e *ExpressionStatement) stmtTag() {}

// IntegerLiteral holds a parsed int64.
type IntegerLiteral struct{ Value int64 }

func (i *IntegerLiteral) nodeTag() {}
func (i *IntegerLiteral) exprTag() {}

// FloatLiteral holds a parsed float64.
type FloatLiteral struct{ Value float64 }

func (f *FloatLiteral) nodeTag() {}
func (f *FloatLiteral) exprTag() {}

// BooleanLiteral holds true or false.
type BooleanLiteral struct{ Value bool }

func (b *BooleanLiteral) nodeTag() {}
func (b *BooleanLiteral) exprTag() {}

// StringLiteral holds a quoted string value.
type StringLiteral struct{ Value string }

func (s *StringLiteral) nodeTag() {}
func (s *StringLiteral) exprTag() {}

// NullLiteral is the literal null keyword.
type NullLiteral struct{}

func (n *NullLiteral) nodeTag() {}
func (n *NullLiteral) exprTag() {}

// Identifier is a variable reference.
type Identifier struct{ Name string }

func (i *Identifier) nodeTag() {}
func (i *Identifier) exprTag() {}

// PrefixExpression applies a unary operator to Right.
type PrefixExpression struct {
	Operator string
	Right    Expression
}

func (p *PrefixExpression) nodeTag() {}
func (p *PrefixExpression) exprTag() {}

// InfixExpression applies Operator between Left and Right.
type InfixExpression struct {
	Left     Expression
	Operator string
	Right    Expression
}

func (i *InfixExpression) nodeTag() {}
func (i *InfixExpression) exprTag() {}

// IfExpression evaluates Condition and branches on truthiness.
// Alternative is nil when there is no else branch.
type IfExpression struct {
	Condition   Expression
	Consequence *BlockStatement
	Alternative *BlockStatement
}

func (i *IfExpression) nodeTag() {}
func (i *IfExpression) exprTag() {}

// FunctionLiteral captures Parameters and Body. Eval wraps it in a Function
// object that also captures the current Environment.
type FunctionLiteral struct {
	Parameters []string
	Body       *BlockStatement
}

func (f *FunctionLiteral) nodeTag() {}
func (f *FunctionLiteral) exprTag() {}

// CallExpression applies Function to Arguments.
type CallExpression struct {
	Function  Expression
	Arguments []Expression
}

func (c *CallExpression) nodeTag() {}
func (c *CallExpression) exprTag() {}
```

Create `object.go`:

```go
package evaluator

import (
	"fmt"
	"strings"
)

// ObjectType is the string tag identifying a runtime value's kind.
type ObjectType string

const (
	IntegerType  ObjectType = "INTEGER"
	FloatType    ObjectType = "FLOAT"
	BooleanType  ObjectType = "BOOLEAN"
	StringType   ObjectType = "STRING"
	NullType     ObjectType = "NULL"
	ReturnType   ObjectType = "RETURN_VALUE"
	ErrorType    ObjectType = "ERROR"
	FunctionType ObjectType = "FUNCTION"
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

// Float wraps float64.
type Float struct{ Value float64 }

func (f *Float) Type() ObjectType { return FloatType }
func (f *Float) Inspect() string  { return fmt.Sprintf("%g", f.Value) }

// Boolean wraps bool. The evaluator uses two package-level singletons
// (TRUE and FALSE) so boolean equality can be tested with pointer ==.
type Boolean struct{ Value bool }

func (b *Boolean) Type() ObjectType { return BooleanType }
func (b *Boolean) Inspect() string  { return fmt.Sprintf("%t", b.Value) }

// String wraps a Go string.
type String struct{ Value string }

func (s *String) Type() ObjectType { return StringType }
func (s *String) Inspect() string  { return s.Value }

// Null is the singleton null value.
type Null struct{}

func (n *Null) Type() ObjectType { return NullType }
func (n *Null) Inspect() string  { return "null" }

// ReturnValue wraps a value so it propagates through block evaluation. It is
// unwrapped at the function-call boundary, not inside nested blocks.
type ReturnValue struct{ Value Object }

func (r *ReturnValue) Type() ObjectType { return ReturnType }
func (r *ReturnValue) Inspect() string  { return r.Value.Inspect() }

// Error represents a runtime error. Errors propagate like ReturnValue but are
// never unwrapped automatically: the caller must check isError.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ErrorType }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Function stores parameters, body, and the closure environment captured at
// definition time. The Env pointer is what makes closures work: when called, a
// new scope is created whose outer pointer is this Env, not the caller's.
type Function struct {
	Parameters []string
	Body       *BlockStatement
	Env        *Environment
}

func (f *Function) Type() ObjectType { return FunctionType }
func (f *Function) Inspect() string {
	return fmt.Sprintf("fn(%s) { ... }", strings.Join(f.Parameters, ", "))
}
```

Create `env.go`:

```go
package evaluator

// Environment maps variable names to Objects and forms a linked chain of
// scopes. The innermost scope delegates unknown lookups to its outer scope,
// implementing lexical scoping without copying bindings.
type Environment struct {
	store map[string]Object
	outer *Environment
}

// NewEnvironment returns a fresh top-level environment with no outer scope.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// NewEnclosedEnvironment returns a new scope nested inside outer. Used for
// function calls so bindings made inside the function do not leak out.
func NewEnclosedEnvironment(outer *Environment) *Environment {
	e := NewEnvironment()
	e.outer = outer
	return e
}

// Get looks up name in the current scope, then walks outward until found or
// the chain is exhausted.
func (e *Environment) Get(name string) (Object, bool) {
	obj, ok := e.store[name]
	if !ok && e.outer != nil {
		return e.outer.Get(name)
	}
	return obj, ok
}

// Set creates or updates name in the current (innermost) scope only. It never
// modifies an outer scope, preventing action at a distance.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}
```

Create `eval.go`:

```go
package evaluator

import "fmt"

// Singletons for the most common values. Using pointers means boolean equality
// can be tested with == instead of a field comparison.
var (
	TRUE  = &Boolean{Value: true}
	FALSE = &Boolean{Value: false}
	NULL  = &Null{}
)

func nativeBool(b bool) *Boolean {
	if b {
		return TRUE
	}
	return FALSE
}

// isError reports whether obj carries a runtime error that must short-circuit
// the surrounding expression.
func isError(obj Object) bool {
	return obj != nil && obj.Type() == ErrorType
}

// isTruthy implements Monkey truthiness: only false and null are falsy. 0 and
// "" are truthy (unlike Python).
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

// Eval recursively evaluates node in env and returns the resulting Object. It
// never panics: all runtime errors are returned as *Error values.
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
	case *ReturnStatement:
		val := Eval(n.Value, env)
		if isError(val) {
			return val
		}
		return &ReturnValue{Value: val}
	case *IntegerLiteral:
		return &Integer{Value: n.Value}
	case *FloatLiteral:
		return &Float{Value: n.Value}
	case *BooleanLiteral:
		return nativeBool(n.Value)
	case *StringLiteral:
		return &String{Value: n.Value}
	case *NullLiteral:
		return NULL
	case *Identifier:
		return evalIdentifier(n, env)
	case *PrefixExpression:
		right := Eval(n.Right, env)
		if isError(right) {
			return right
		}
		return evalPrefixExpression(n.Operator, right)
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
	case *FunctionLiteral:
		return &Function{Parameters: n.Parameters, Body: n.Body, Env: env}
	case *CallExpression:
		fn := Eval(n.Function, env)
		if isError(fn) {
			return fn
		}
		args, err := evalExpressions(n.Arguments, env)
		if err != nil {
			return err
		}
		return applyFunction(fn, args)
	}
	return newError("unknown node type: %T", node)
}

// evalProgram evaluates top-level statements and unwraps ReturnValue. An error
// short-circuits immediately.
func evalProgram(prog *Program, env *Environment) Object {
	var result Object
	for _, stmt := range prog.Statements {
		result = Eval(stmt, env)
		switch r := result.(type) {
		case *ReturnValue:
			return r.Value // unwrap at top level
		case *Error:
			return result
		}
	}
	return result
}

// evalBlockStatement evaluates statements in a block but does NOT unwrap
// ReturnValue: it relays the signal up to the function-call handler.
func evalBlockStatement(block *BlockStatement, env *Environment) Object {
	var result Object
	for _, stmt := range block.Statements {
		result = Eval(stmt, env)
		if result != nil {
			rt := result.Type()
			if rt == ReturnType || rt == ErrorType {
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

// evalExpressions evaluates a list of expressions left-to-right. It returns nil
// plus the first error on failure.
func evalExpressions(exprs []Expression, env *Environment) ([]Object, *Error) {
	result := make([]Object, 0, len(exprs))
	for _, e := range exprs {
		val := Eval(e, env)
		if isError(val) {
			return nil, val.(*Error)
		}
		result = append(result, val)
	}
	return result, nil
}

func evalPrefixExpression(op string, right Object) Object {
	switch op {
	case "!":
		return nativeBool(!isTruthy(right))
	case "-":
		switch r := right.(type) {
		case *Integer:
			return &Integer{Value: -r.Value}
		case *Float:
			return &Float{Value: -r.Value}
		}
		return newError("unknown operator: -%s", right.Type())
	}
	return newError("unknown prefix operator: %s", op)
}

func evalInfixExpression(op string, left, right Object) Object {
	switch {
	case left.Type() == IntegerType && right.Type() == IntegerType:
		return evalIntegerInfix(op, left.(*Integer), right.(*Integer))
	case left.Type() == FloatType || right.Type() == FloatType:
		return evalFloatInfix(op, toFloat(left), toFloat(right))
	case left.Type() == StringType && right.Type() == StringType:
		return evalStringInfix(op, left.(*String), right.(*String))
	case op == "==":
		return nativeBool(left == right) // singleton pointer equality
	case op == "!=":
		return nativeBool(left != right)
	case left.Type() != right.Type():
		return newError("type mismatch: %s %s %s", left.Type(), op, right.Type())
	}
	return newError("unknown operator: %s %s %s", left.Type(), op, right.Type())
}

func evalIntegerInfix(op string, left, right *Integer) Object {
	l, r := left.Value, right.Value
	switch op {
	case "+":
		return &Integer{Value: l + r}
	case "-":
		return &Integer{Value: l - r}
	case "*":
		return &Integer{Value: l * r}
	case "/":
		if r == 0 {
			return newError("division by zero")
		}
		return &Integer{Value: l / r}
	case "%":
		if r == 0 {
			return newError("modulo by zero")
		}
		return &Integer{Value: l % r}
	case "<":
		return nativeBool(l < r)
	case ">":
		return nativeBool(l > r)
	case "<=":
		return nativeBool(l <= r)
	case ">=":
		return nativeBool(l >= r)
	case "==":
		return nativeBool(l == r)
	case "!=":
		return nativeBool(l != r)
	}
	return newError("unknown operator: INTEGER %s INTEGER", op)
}

func evalFloatInfix(op string, left, right *Float) Object {
	l, r := left.Value, right.Value
	switch op {
	case "+":
		return &Float{Value: l + r}
	case "-":
		return &Float{Value: l - r}
	case "*":
		return &Float{Value: l * r}
	case "/":
		if r == 0 {
			return newError("division by zero")
		}
		return &Float{Value: l / r}
	case "<":
		return nativeBool(l < r)
	case ">":
		return nativeBool(l > r)
	case "<=":
		return nativeBool(l <= r)
	case ">=":
		return nativeBool(l >= r)
	case "==":
		return nativeBool(l == r)
	case "!=":
		return nativeBool(l != r)
	}
	return newError("unknown operator: FLOAT %s FLOAT", op)
}

func evalStringInfix(op string, left, right *String) Object {
	switch op {
	case "+":
		return &String{Value: left.Value + right.Value}
	case "==":
		return nativeBool(left.Value == right.Value)
	case "!=":
		return nativeBool(left.Value != right.Value)
	case "<":
		return nativeBool(left.Value < right.Value)
	case ">":
		return nativeBool(left.Value > right.Value)
	}
	return newError("unknown operator: STRING %s STRING", op)
}

// toFloat converts an Integer or Float to *Float for mixed-type arithmetic.
func toFloat(obj Object) *Float {
	switch o := obj.(type) {
	case *Integer:
		return &Float{Value: float64(o.Value)}
	case *Float:
		return o
	}
	return &Float{}
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

// applyFunction creates an enclosed environment from the closure's captured Env,
// binds the parameters, evaluates the body, and unwraps any ReturnValue at the
// function boundary.
func applyFunction(fn Object, args []Object) Object {
	f, ok := fn.(*Function)
	if !ok {
		return newError("not a function: %s", fn.Type())
	}
	if len(args) != len(f.Parameters) {
		return newError("wrong number of arguments: want %d, got %d",
			len(f.Parameters), len(args))
	}
	enclosed := NewEnclosedEnvironment(f.Env)
	for i, param := range f.Parameters {
		enclosed.Set(param, args[i])
	}
	result := evalBlockStatement(f.Body, enclosed)
	if rv, ok := result.(*ReturnValue); ok {
		return rv.Value // unwrap at function boundary
	}
	return result
}
```

### The runnable demo

The demo builds four ASTs by hand and evaluates them: an arithmetic expression with operator precedence baked into its shape, a `let` binding followed by a lookup, a curried closure `fn(x){fn(y){x+y}}(5)(10)`, and a division by zero. The closure double-call is expressed entirely at the AST level (a `CallExpression` whose `Function` is another `CallExpression`) so no runtime `Object` is ever placed in an `Expression` slot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/evaluator"
)

func main() {
	// 1. Arithmetic with precedence: (5 + 10*2 + 15/3) * 2 + -10 == 50.
	env := evaluator.NewEnvironment()
	arithmetic := &evaluator.InfixExpression{
		Left: &evaluator.InfixExpression{
			Left: &evaluator.InfixExpression{
				Left: &evaluator.InfixExpression{
					Left:     &evaluator.IntegerLiteral{Value: 5},
					Operator: "+",
					Right: &evaluator.InfixExpression{
						Left:     &evaluator.IntegerLiteral{Value: 10},
						Operator: "*",
						Right:    &evaluator.IntegerLiteral{Value: 2},
					},
				},
				Operator: "+",
				Right: &evaluator.InfixExpression{
					Left:     &evaluator.IntegerLiteral{Value: 15},
					Operator: "/",
					Right:    &evaluator.IntegerLiteral{Value: 3},
				},
			},
			Operator: "*",
			Right:    &evaluator.IntegerLiteral{Value: 2},
		},
		Operator: "+",
		Right: &evaluator.PrefixExpression{
			Operator: "-",
			Right:    &evaluator.IntegerLiteral{Value: 10},
		},
	}
	fmt.Println("arithmetic:", evaluator.Eval(arithmetic, env).Inspect())

	// 2. Variable binding.
	bindEnv := evaluator.NewEnvironment()
	p := &evaluator.Program{Statements: []evaluator.Statement{
		&evaluator.LetStatement{
			Name:  "answer",
			Value: &evaluator.IntegerLiteral{Value: 42},
		},
		&evaluator.ExpressionStatement{Expr: &evaluator.Identifier{Name: "answer"}},
	}}
	fmt.Println("binding:", evaluator.Eval(p, bindEnv).Inspect())

	// 3. Closure: fn(x){fn(y){x+y}}(5)(10) == 15. The double call is built at
	// the AST level so no runtime Object sits in Expression position.
	closureEnv := evaluator.NewEnvironment()
	innerFn := &evaluator.FunctionLiteral{
		Parameters: []string{"y"},
		Body: &evaluator.BlockStatement{Statements: []evaluator.Statement{
			&evaluator.ExpressionStatement{Expr: &evaluator.InfixExpression{
				Left:     &evaluator.Identifier{Name: "x"},
				Operator: "+",
				Right:    &evaluator.Identifier{Name: "y"},
			}},
		}},
	}
	outerFn := &evaluator.FunctionLiteral{
		Parameters: []string{"x"},
		Body: &evaluator.BlockStatement{Statements: []evaluator.Statement{
			&evaluator.ExpressionStatement{Expr: innerFn},
		}},
	}
	call5 := &evaluator.CallExpression{
		Function:  outerFn,
		Arguments: []evaluator.Expression{&evaluator.IntegerLiteral{Value: 5}},
	}
	call10 := &evaluator.CallExpression{
		Function:  call5,
		Arguments: []evaluator.Expression{&evaluator.IntegerLiteral{Value: 10}},
	}
	fmt.Println("closure:", evaluator.Eval(call10, closureEnv).Inspect())

	// 4. Runtime error propagation, no panic.
	errNode := &evaluator.InfixExpression{
		Left:     &evaluator.IntegerLiteral{Value: 5},
		Operator: "/",
		Right:    &evaluator.IntegerLiteral{Value: 0},
	}
	fmt.Println("error:", evaluator.Eval(errNode, evaluator.NewEnvironment()).Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
arithmetic: 50
binding: 42
closure: 15
error: ERROR: division by zero
```

### Tests

The tests build minimal ASTs through small helper constructors and assert on the concrete result type. `TestRecursion` exercises the per-call enclosed environment via `fact(10) == 3628800`; `TestClosureCapture` proves the captured definition environment; `TestReturnExitsEarly` proves the relay-then-unwrap discipline; `TestErrorPropagation` proves `isError` short-circuits before a bad operand reaches an operator. The `Example` functions are verified by `go test` against their `// Output:` comments.

Create `eval_test.go`:

```go
package evaluator

import (
	"fmt"
	"testing"
)

// --- test helpers ---

func prog(stmts ...Statement) *Program { return &Program{Statements: stmts} }

func intLit(v int64) *IntegerLiteral { return &IntegerLiteral{Value: v} }
func boolLit(v bool) *BooleanLiteral { return &BooleanLiteral{Value: v} }
func strLit(v string) *StringLiteral { return &StringLiteral{Value: v} }
func ident(name string) *Identifier  { return &Identifier{Name: name} }

func infix(left Expression, op string, right Expression) *InfixExpression {
	return &InfixExpression{Left: left, Operator: op, Right: right}
}

func prefix(op string, right Expression) *PrefixExpression {
	return &PrefixExpression{Operator: op, Right: right}
}

func exprStmt(e Expression) *ExpressionStatement {
	return &ExpressionStatement{Expr: e}
}

func letStmt(name string, val Expression) *LetStatement {
	return &LetStatement{Name: name, Value: val}
}

func blk(stmts ...Statement) *BlockStatement {
	return &BlockStatement{Statements: stmts}
}

func newEnv() *Environment { return NewEnvironment() }

// --- tests ---

func TestIntegerArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		node Expression
		want int64
	}{
		{infix(intLit(5), "+", intLit(3)), 8},
		{infix(intLit(10), "-", intLit(4)), 6},
		{infix(intLit(3), "*", intLit(7)), 21},
		{infix(intLit(15), "/", intLit(3)), 5},
		{infix(intLit(10), "%", intLit(3)), 1},
		// (5 + 10*2 + 15/3) * 2 + -10 == 50
		{
			infix(
				infix(
					infix(
						infix(intLit(5), "+", infix(intLit(10), "*", intLit(2))),
						"+",
						infix(intLit(15), "/", intLit(3)),
					),
					"*", intLit(2),
				),
				"+", prefix("-", intLit(10)),
			),
			50,
		},
	}

	for _, tc := range cases {
		got := Eval(exprStmt(tc.node), newEnv())
		i, ok := got.(*Integer)
		if !ok {
			t.Fatalf("Eval(%v) = %T(%v), want *Integer", tc.node, got, got)
		}
		if i.Value != tc.want {
			t.Errorf("got %d, want %d", i.Value, tc.want)
		}
	}
}

func TestDivisionByZero(t *testing.T) {
	t.Parallel()

	got := Eval(exprStmt(infix(intLit(5), "/", intLit(0))), newEnv())
	e, ok := got.(*Error)
	if !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
	if e.Message != "division by zero" {
		t.Errorf("message = %q, want %q", e.Message, "division by zero")
	}
}

func TestStringConcatenation(t *testing.T) {
	t.Parallel()

	got := Eval(exprStmt(infix(strLit("hello"), "+", strLit(", world"))), newEnv())
	s, ok := got.(*String)
	if !ok {
		t.Fatalf("Eval = %T, want *String", got)
	}
	if s.Value != "hello, world" {
		t.Errorf("Value = %q, want %q", s.Value, "hello, world")
	}
}

// TestTruthiness documents the Monkey truthiness contract: 0 and "" are truthy;
// only false and null are falsy.
func TestTruthiness(t *testing.T) {
	t.Parallel()

	truthy := []Expression{intLit(0), strLit(""), boolLit(true), intLit(42)}
	falsy := []Expression{boolLit(false), &NullLiteral{}}

	for _, e := range truthy {
		if !isTruthy(Eval(exprStmt(e), newEnv())) {
			t.Errorf("isTruthy(%v) = false, want true", e)
		}
	}
	for _, e := range falsy {
		if isTruthy(Eval(exprStmt(e), newEnv())) {
			t.Errorf("isTruthy(%v) = true, want false", e)
		}
	}
}

func TestBooleanComparisons(t *testing.T) {
	t.Parallel()

	cases := []struct {
		node Expression
		want bool
	}{
		{infix(intLit(1), "==", intLit(1)), true},
		{infix(intLit(1), "==", intLit(2)), false},
		{infix(intLit(1), "!=", intLit(2)), true},
		{infix(boolLit(true), "==", boolLit(true)), true},
		{infix(boolLit(true), "==", boolLit(false)), false},
		{prefix("!", boolLit(true)), false},
		{prefix("!", boolLit(false)), true},
		{prefix("!", &NullLiteral{}), true},
	}

	for _, tc := range cases {
		got := Eval(exprStmt(tc.node), newEnv())
		b, ok := got.(*Boolean)
		if !ok {
			t.Fatalf("Eval = %T, want *Boolean", got)
		}
		if b.Value != tc.want {
			t.Errorf("got %v, want %v", b.Value, tc.want)
		}
	}
}

func TestLetAndIdentifier(t *testing.T) {
	t.Parallel()

	env := newEnv()
	p := prog(
		letStmt("x", intLit(42)),
		exprStmt(ident("x")),
	)
	got := Eval(p, env)
	i, ok := got.(*Integer)
	if !ok {
		t.Fatalf("Eval = %T, want *Integer", got)
	}
	if i.Value != 42 {
		t.Errorf("x = %d, want 42", i.Value)
	}
}

func TestUndefinedVariable(t *testing.T) {
	t.Parallel()

	got := Eval(exprStmt(ident("notDefined")), newEnv())
	if _, ok := got.(*Error); !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
}

func TestIfElse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cond Expression
		want int64
	}{
		{boolLit(true), 1},
		{boolLit(false), 2},
	}

	for _, tc := range cases {
		node := &IfExpression{
			Condition:   tc.cond,
			Consequence: blk(exprStmt(intLit(1))),
			Alternative: blk(exprStmt(intLit(2))),
		}
		got := Eval(node, newEnv())
		i, ok := got.(*Integer)
		if !ok {
			t.Fatalf("Eval = %T, want *Integer", got)
		}
		if i.Value != tc.want {
			t.Errorf("got %d, want %d", i.Value, tc.want)
		}
	}
}

// TestReturnExitsEarly verifies return stops execution inside a function body:
// fn() { return 10; 99 }() must yield 10, not 99.
func TestReturnExitsEarly(t *testing.T) {
	t.Parallel()

	fnLit := &FunctionLiteral{
		Parameters: []string{},
		Body: blk(
			&ReturnStatement{Value: intLit(10)},
			exprStmt(intLit(99)),
		),
	}
	call := &CallExpression{Function: fnLit, Arguments: []Expression{}}
	got := Eval(call, newEnv())
	i, ok := got.(*Integer)
	if !ok {
		t.Fatalf("Eval = %T, want *Integer", got)
	}
	if i.Value != 10 {
		t.Errorf("got %d, want 10", i.Value)
	}
}

// TestClosureCapture verifies fn(x){ fn(y){ x+y } }(5)(10) == 15.
func TestClosureCapture(t *testing.T) {
	t.Parallel()

	env := newEnv()

	inner := &FunctionLiteral{
		Parameters: []string{"y"},
		Body:       blk(exprStmt(infix(ident("x"), "+", ident("y")))),
	}
	outer := &FunctionLiteral{
		Parameters: []string{"x"},
		Body:       blk(exprStmt(inner)),
	}
	p := prog(
		letStmt("adder", outer),
		letStmt("add5", &CallExpression{
			Function:  ident("adder"),
			Arguments: []Expression{intLit(5)},
		}),
		exprStmt(&CallExpression{
			Function:  ident("add5"),
			Arguments: []Expression{intLit(10)},
		}),
	)
	got := Eval(p, env)
	i, ok := got.(*Integer)
	if !ok {
		t.Fatalf("Eval = %T, want *Integer", got)
	}
	if i.Value != 15 {
		t.Errorf("add5(10) = %d, want 15", i.Value)
	}
}

// TestRecursion verifies fact(10) == 3628800, exercising a fresh enclosed
// environment per call.
func TestRecursion(t *testing.T) {
	t.Parallel()

	env := newEnv()

	// fn(n) { if (n < 2) { 1 } else { n * fact(n-1) } }
	factBody := blk(exprStmt(&IfExpression{
		Condition:   infix(ident("n"), "<", intLit(2)),
		Consequence: blk(exprStmt(intLit(1))),
		Alternative: blk(exprStmt(
			infix(ident("n"), "*", &CallExpression{
				Function:  ident("fact"),
				Arguments: []Expression{infix(ident("n"), "-", intLit(1))},
			}),
		)),
	}))

	p := prog(
		letStmt("fact", &FunctionLiteral{Parameters: []string{"n"}, Body: factBody}),
		exprStmt(&CallExpression{
			Function:  ident("fact"),
			Arguments: []Expression{intLit(10)},
		}),
	)
	got := Eval(p, env)
	i, ok := got.(*Integer)
	if !ok {
		t.Fatalf("Eval = %T, want *Integer", got)
	}
	if i.Value != 3628800 {
		t.Errorf("fact(10) = %d, want 3628800", i.Value)
	}
}

// TestTypeMismatch verifies INTEGER + BOOLEAN returns *Error.
func TestTypeMismatch(t *testing.T) {
	t.Parallel()

	got := Eval(exprStmt(infix(intLit(1), "+", boolLit(true))), newEnv())
	if _, ok := got.(*Error); !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
}

// TestErrorPropagation verifies (1/0)+5 returns the division error rather than
// trying to add 5 to an error value.
func TestErrorPropagation(t *testing.T) {
	t.Parallel()

	got := Eval(
		exprStmt(infix(infix(intLit(1), "/", intLit(0)), "+", intLit(5))),
		newEnv(),
	)
	e, ok := got.(*Error)
	if !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
	if e.Message != "division by zero" {
		t.Errorf("message = %q", e.Message)
	}
}

// TestArityMismatch verifies calling a function with the wrong number of
// arguments returns an *Error.
func TestArityMismatch(t *testing.T) {
	t.Parallel()

	fnLit := &FunctionLiteral{
		Parameters: []string{"x", "y"},
		Body:       blk(exprStmt(intLit(0))),
	}
	call := &CallExpression{Function: fnLit, Arguments: []Expression{intLit(1)}}
	got := Eval(call, newEnv())
	if _, ok := got.(*Error); !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
}

// TestMixedFloatArithmetic verifies integer + float promotes to float.
func TestMixedFloatArithmetic(t *testing.T) {
	t.Parallel()

	node := &InfixExpression{
		Left:     &IntegerLiteral{Value: 3},
		Operator: "+",
		Right:    &FloatLiteral{Value: 1.5},
	}
	got := Eval(node, newEnv())
	f, ok := got.(*Float)
	if !ok {
		t.Fatalf("Eval = %T, want *Float", got)
	}
	if f.Value != 4.5 {
		t.Errorf("3 + 1.5 = %g, want 4.5", f.Value)
	}
}

// --- Example functions (auto-verified by go test) ---

func ExampleEval_arithmetic() {
	env := NewEnvironment()
	node := &InfixExpression{
		Left:     &IntegerLiteral{Value: 6},
		Operator: "*",
		Right:    &IntegerLiteral{Value: 7},
	}
	fmt.Println(Eval(node, env).Inspect())
	// Output: 42
}

func ExampleEval_closure() {
	env := NewEnvironment()

	inner := &FunctionLiteral{
		Parameters: []string{"y"},
		Body: &BlockStatement{Statements: []Statement{
			&ExpressionStatement{Expr: &InfixExpression{
				Left:     &Identifier{Name: "x"},
				Operator: "+",
				Right:    &Identifier{Name: "y"},
			}},
		}},
	}
	outer := &FunctionLiteral{
		Parameters: []string{"x"},
		Body: &BlockStatement{Statements: []Statement{
			&ExpressionStatement{Expr: inner},
		}},
	}
	call5 := &CallExpression{
		Function:  outer,
		Arguments: []Expression{&IntegerLiteral{Value: 5}},
	}
	call10 := &CallExpression{
		Function:  call5,
		Arguments: []Expression{&IntegerLiteral{Value: 10}},
	}
	fmt.Println(Eval(call10, env).Inspect())
	// Output: 15
}

func ExampleEval_runtimeError() {
	env := NewEnvironment()
	node := &InfixExpression{
		Left:     &IntegerLiteral{Value: 1},
		Operator: "/",
		Right:    &IntegerLiteral{Value: 0},
	}
	fmt.Println(Eval(node, env).Inspect())
	// Output: ERROR: division by zero
}
```

## Review

The evaluator is correct when it is a total function: every input node produces an `Object`, never a panic. Confirm the four load-bearing behaviors. `fact(10)` returns `3628800`, which can only happen if each recursive call gets its own enclosed environment rather than sharing one `n`. `fn(x){fn(y){x+y}}(5)(10)` returns `15`, which can only happen if the inner function captured the definition environment holding `x = 5`. `fn() { return 10; 99 }()` returns `10`, which proves the `ReturnValue` relayed through the block and was unwrapped only at the call boundary. And `(1/0)+5` returns `ERROR: division by zero`, which proves `isError` short-circuited before the error reached the `+` handler. Truthiness is pinned by `TestTruthiness`: `0` and `""` are truthy, only `false` and `null` are falsy.

The high-leverage mistakes are all about boundaries. Unwrapping `ReturnValue` inside `evalBlockStatement` makes `return` escape only one block instead of the whole function. Skipping an `isError` check between two child evaluations lets an error flow into an operator and be masked by a confusing "unknown operator". Building `applyFunction`'s enclosed environment from the caller's `env` instead of `f.Env` breaks closures with a spurious "identifier not found". Reusing one environment across recursive calls corrupts the recursion. Each of these passes casual eyeballing and is caught only by the targeted tests above.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — the direct model for this evaluator's object system, environment, and Eval design.
- [Crafting Interpreters: Evaluating Expressions, Robert Nystrom](https://craftinginterpreters.com/evaluating-expressions.html) — the tree-walking evaluation chapter with a Java reference implementation.
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the dispatch mechanism used throughout Eval.
- [go/ast package](https://pkg.go.dev/go/ast) — the marker-interface pattern the AST node tags imitate.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-loops-and-control-flow.md](02-loops-and-control-flow.md)
