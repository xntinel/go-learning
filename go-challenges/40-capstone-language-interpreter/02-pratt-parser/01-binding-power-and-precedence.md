# Exercise 1: Binding Power and the Pratt Loop

Every expression a Monkey program contains has to become a tree, and that tree has to nest the way the operators say it should: `1 + 2 * 3` is an addition whose right operand is a multiplication, not a multiplication whose left operand is an addition. This exercise builds the engine that gets that right — a Pratt (top-down operator precedence) parser whose entire precedence and associativity behaviour lives in one integer table and one loop. You write a lexer for the Monkey tokens, an AST, and a parser whose `parseExpression(minBP)` consumes operators exactly as long as they bind tighter than the current threshold, with left- and right-associative operators differing by a single `- 1`.

This module is fully self-contained. It begins with its own `go mod init`, defines its own lexer and AST, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go         TokenType, Token, Lexer for the Monkey tokens
ast.go           Node/Expression/Statement interfaces and every concrete node
parser.go        binding-power table, parseExpression, prefix/infix parse functions, statements
parser_test.go   precedence, associativity, calls, index, statements, Example outputs
cmd/
  demo/
    main.go      parse a small Monkey program and print its fully parenthesized AST
```

- Files: `token.go`, `ast.go`, `parser.go`, `parser_test.go`, `cmd/demo/main.go`.
- Implement: `New(src string) *Parser`, `(*Parser).Parse() *Program`, `(*Parser).Errors()`, the `infixBP` precedence table, `parseExpression`, and the full set of prefix and infix parse functions.
- Test: `parser_test.go` pins precedence and associativity as parenthesized strings, checks calls, indexing, `if`, `fn`, and collection literals, and uses `Example` functions whose `// Output:` lines are auto-verified.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

### The token layer

The parser consumes tokens, so the first file defines them and the lexer that produces them. The lexer takes the source string and returns `Token` values on demand through `Next`; the parser pulls from it one token at a time. Two facts about the lexer matter for the parser above it. First, two-character operators (`**`, `<=`, `==`, `&&`, ...) are matched before single-character ones, so `**` is never mis-scanned as two separate `*` tokens — the maximal-munch rule that keeps exponentiation a single operator. Second, every token carries its line and column, which is what lets a parse error point at a location rather than a vague "somewhere."

Create `token.go`:

```go
package parser

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenType identifies a terminal symbol in the Monkey language grammar.
type TokenType int

const (
	EOF     TokenType = iota // end of input
	ILLEGAL                  // unrecognised byte
	INT                      // 42
	STRING                   // "hello"
	IDENT                    // foo

	// keywords
	TRUE   // true
	FALSE  // false
	NIL    // nil
	LET    // let
	CONST  // const
	RETURN // return
	IF     // if
	ELSE   // else
	FN     // fn

	// punctuation
	LPAREN    // (
	RPAREN    // )
	LBRACE    // {
	RBRACE    // }
	LBRACKET  // [
	RBRACKET  // ]
	COMMA     // ,
	SEMICOLON // ;
	COLON     // :

	// operators
	PLUS     // +
	MINUS    // -
	STAR     // *
	SLASH    // /
	PERCENT  // %
	STARSTAR // **
	BANG     // !
	TILDE    // ~
	LT       // <
	GT       // >
	LTEQ     // <=
	GTEQ     // >=
	EQEQ     // ==
	BANGEQ   // !=
	AMPAMP   // &&
	PIPEPIPE // ||
	ASSIGN   // =
	DOTDOT   // ..
)

var tokenNames = map[TokenType]string{
	EOF: "EOF", ILLEGAL: "ILLEGAL", INT: "INT", STRING: "STRING", IDENT: "IDENT",
	TRUE: "true", FALSE: "false", NIL: "nil",
	LET: "let", CONST: "const", RETURN: "return", IF: "if", ELSE: "else", FN: "fn",
	LPAREN: "(", RPAREN: ")", LBRACE: "{", RBRACE: "}", LBRACKET: "[", RBRACKET: "]",
	COMMA: ",", SEMICOLON: ";", COLON: ":",
	PLUS: "+", MINUS: "-", STAR: "*", SLASH: "/", PERCENT: "%", STARSTAR: "**",
	BANG: "!", TILDE: "~", LT: "<", GT: ">", LTEQ: "<=", GTEQ: ">=",
	EQEQ: "==", BANGEQ: "!=", AMPAMP: "&&", PIPEPIPE: "||", ASSIGN: "=", DOTDOT: "..",
}

func (t TokenType) String() string {
	if s, ok := tokenNames[t]; ok {
		return s
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

var keywords = map[string]TokenType{
	"true":   TRUE,
	"false":  FALSE,
	"nil":    NIL,
	"let":    LET,
	"const":  CONST,
	"return": RETURN,
	"if":     IF,
	"else":   ELSE,
	"fn":     FN,
}

// Token is one terminal symbol with its source text and position.
type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Col     int
}

func (t Token) String() string {
	return fmt.Sprintf("Token{%s %q %d:%d}", t.Type, t.Literal, t.Line, t.Col)
}

// Lexer scans a source string into a stream of tokens.
type Lexer struct {
	src  string
	pos  int // current byte index
	line int
	col  int
}

// NewLexer returns a Lexer ready to scan src.
func NewLexer(src string) *Lexer {
	return &Lexer{src: src, line: 1, col: 1}
}

// Next returns the next token, advancing past it.
func (l *Lexer) Next() Token {
	l.skipWhitespace()
	if l.pos >= len(l.src) {
		return Token{Type: EOF, Line: l.line, Col: l.col}
	}

	startLine, startCol := l.line, l.col
	ch := l.src[l.pos]

	// two-char operators first
	if l.pos+1 < len(l.src) {
		two := l.src[l.pos : l.pos+2]
		switch two {
		case "**":
			return l.tok2(STARSTAR, two, startLine, startCol)
		case "<=":
			return l.tok2(LTEQ, two, startLine, startCol)
		case ">=":
			return l.tok2(GTEQ, two, startLine, startCol)
		case "==":
			return l.tok2(EQEQ, two, startLine, startCol)
		case "!=":
			return l.tok2(BANGEQ, two, startLine, startCol)
		case "&&":
			return l.tok2(AMPAMP, two, startLine, startCol)
		case "||":
			return l.tok2(PIPEPIPE, two, startLine, startCol)
		case "..":
			return l.tok2(DOTDOT, two, startLine, startCol)
		}
	}

	// single-char tokens
	singles := map[byte]TokenType{
		'(': LPAREN, ')': RPAREN, '{': LBRACE, '}': RBRACE,
		'[': LBRACKET, ']': RBRACKET, ',': COMMA, ';': SEMICOLON, ':': COLON,
		'+': PLUS, '-': MINUS, '*': STAR, '/': SLASH, '%': PERCENT,
		'!': BANG, '~': TILDE, '<': LT, '>': GT, '=': ASSIGN,
	}
	if tt, ok := singles[ch]; ok {
		l.advance()
		return Token{Type: tt, Literal: string(ch), Line: startLine, Col: startCol}
	}

	if ch == '"' {
		return l.readString(startLine, startCol)
	}
	if isDigit(ch) {
		return l.readInt(startLine, startCol)
	}
	if isLetter(ch) {
		return l.readIdent(startLine, startCol)
	}

	// unknown byte
	l.advance()
	return Token{Type: ILLEGAL, Literal: string(ch), Line: startLine, Col: startCol}
}

func (l *Lexer) tok2(tt TokenType, lit string, line, col int) Token {
	l.pos += 2
	l.col += 2
	return Token{Type: tt, Literal: lit, Line: line, Col: col}
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

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t' ||
		l.src[l.pos] == '\n' || l.src[l.pos] == '\r') {
		l.advance()
	}
}

func (l *Lexer) readInt(line, col int) Token {
	start := l.pos
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.advance()
	}
	return Token{Type: INT, Literal: l.src[start:l.pos], Line: line, Col: col}
}

func (l *Lexer) readIdent(line, col int) Token {
	start := l.pos
	for l.pos < len(l.src) && (isLetter(l.src[l.pos]) || isDigit(l.src[l.pos])) {
		l.advance()
	}
	lit := l.src[start:l.pos]
	tt := IDENT
	if kw, ok := keywords[strings.ToLower(lit)]; ok {
		tt = kw
	}
	return Token{Type: tt, Literal: lit, Line: line, Col: col}
}

func (l *Lexer) readString(line, col int) Token {
	l.advance() // skip opening "
	var sb strings.Builder
	for l.pos < len(l.src) && l.src[l.pos] != '"' {
		if l.src[l.pos] == '\\' && l.pos+1 < len(l.src) {
			l.advance()
			switch l.src[l.pos] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(l.src[l.pos])
			}
		} else {
			sb.WriteByte(l.src[l.pos])
		}
		l.advance()
	}
	l.advance() // skip closing "
	return Token{Type: STRING, Literal: sb.String(), Line: line, Col: col}
}

func isDigit(ch byte) bool  { return ch >= '0' && ch <= '9' }
func isLetter(ch byte) bool { return ch == '_' || unicode.IsLetter(rune(ch)) }
```

### The AST

Every node implements `Node` (it can stringify itself and report the literal of the token that started it). `Expression` and `Statement` embed `Node` and add an unexported marker method — `expressionNode()` or `statementNode()` — so the type system refuses to let a statement appear where an expression is required and vice versa; the marker methods carry no behaviour, they exist only to make the two interfaces distinct. The `String()` methods are the parser's proof of work: each one emits a fully parenthesized form, so `1 + 2 * 3` stringifies to `(1 + (2 * 3))` and the precedence tree is visible in the text. Those strings are exactly what the tests and the `Example` outputs assert against, which means a precedence bug shows up as a string mismatch rather than as a silent wrong answer.

Create `ast.go`:

```go
package parser

import (
	"fmt"
	"strings"
)

// Node is the base interface for every node in the AST.
type Node interface {
	String() string
	// TokenLiteral returns the literal of the token that started this node,
	// used for debugging.
	TokenLiteral() string
}

// Expression is a Node that produces a value.
type Expression interface {
	Node
	expressionNode()
}

// Statement is a Node that performs an action without producing a value.
type Statement interface {
	Node
	statementNode()
}

// --- Program -----------------------------------------------------------------

// Program is the root of every AST.
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
	var sb strings.Builder
	for i, s := range p.Statements {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(s.String())
	}
	return sb.String()
}

// --- Literals ----------------------------------------------------------------

// IntegerLiteral is an integer constant, e.g., 42.
type IntegerLiteral struct {
	Tok   Token
	Value int64
}

func (il *IntegerLiteral) expressionNode()      {}
func (il *IntegerLiteral) TokenLiteral() string { return il.Tok.Literal }
func (il *IntegerLiteral) String() string       { return il.Tok.Literal }

// StringLiteral is a string constant, e.g., "hello".
type StringLiteral struct {
	Tok   Token
	Value string
}

func (sl *StringLiteral) expressionNode()      {}
func (sl *StringLiteral) TokenLiteral() string { return sl.Tok.Literal }
func (sl *StringLiteral) String() string       { return fmt.Sprintf("%q", sl.Value) }

// BoolLiteral is true or false.
type BoolLiteral struct {
	Tok   Token
	Value bool
}

func (bl *BoolLiteral) expressionNode()      {}
func (bl *BoolLiteral) TokenLiteral() string { return bl.Tok.Literal }
func (bl *BoolLiteral) String() string {
	if bl.Value {
		return "true"
	}
	return "false"
}

// NilLiteral is the nil keyword.
type NilLiteral struct {
	Tok Token
}

func (nl *NilLiteral) expressionNode()      {}
func (nl *NilLiteral) TokenLiteral() string { return nl.Tok.Literal }
func (nl *NilLiteral) String() string       { return "nil" }

// Identifier is a variable or function name.
type Identifier struct {
	Tok  Token
	Name string
}

func (id *Identifier) expressionNode()      {}
func (id *Identifier) TokenLiteral() string { return id.Tok.Literal }
func (id *Identifier) String() string       { return id.Name }

// --- Prefix and Infix --------------------------------------------------------

// PrefixExpression is an operator applied to one operand: !x, -x, ~x.
type PrefixExpression struct {
	Tok      Token
	Operator string
	Right    Expression
}

func (pe *PrefixExpression) expressionNode()      {}
func (pe *PrefixExpression) TokenLiteral() string { return pe.Tok.Literal }
func (pe *PrefixExpression) String() string {
	return "(" + pe.Operator + pe.Right.String() + ")"
}

// InfixExpression is a binary operator: left op right.
type InfixExpression struct {
	Tok      Token
	Left     Expression
	Operator string
	Right    Expression
}

func (ie *InfixExpression) expressionNode()      {}
func (ie *InfixExpression) TokenLiteral() string { return ie.Tok.Literal }
func (ie *InfixExpression) String() string {
	return "(" + ie.Left.String() + " " + ie.Operator + " " + ie.Right.String() + ")"
}

// --- Compound expressions ----------------------------------------------------

// IfExpression is if (cond) { consequence } else { alternative }.
type IfExpression struct {
	Tok         Token
	Condition   Expression
	Consequence *BlockStatement
	Alternative *BlockStatement
}

func (ife *IfExpression) expressionNode()      {}
func (ife *IfExpression) TokenLiteral() string { return ife.Tok.Literal }
func (ife *IfExpression) String() string {
	s := "if (" + ife.Condition.String() + ") " + ife.Consequence.String()
	if ife.Alternative != nil {
		s += " else " + ife.Alternative.String()
	}
	return s
}

// FnLiteral is fn(params) { body }.
type FnLiteral struct {
	Tok    Token
	Params []*Identifier
	Body   *BlockStatement
}

func (fl *FnLiteral) expressionNode()      {}
func (fl *FnLiteral) TokenLiteral() string { return fl.Tok.Literal }
func (fl *FnLiteral) String() string {
	params := make([]string, len(fl.Params))
	for i, p := range fl.Params {
		params[i] = p.String()
	}
	return "fn(" + strings.Join(params, ", ") + ") " + fl.Body.String()
}

// CallExpression is callee(args...).
type CallExpression struct {
	Tok    Token
	Callee Expression
	Args   []Expression
}

func (ce *CallExpression) expressionNode()      {}
func (ce *CallExpression) TokenLiteral() string { return ce.Tok.Literal }
func (ce *CallExpression) String() string {
	args := make([]string, len(ce.Args))
	for i, a := range ce.Args {
		args[i] = a.String()
	}
	return ce.Callee.String() + "(" + strings.Join(args, ", ") + ")"
}

// IndexExpression is expr[index].
type IndexExpression struct {
	Tok   Token
	Left  Expression
	Index Expression
}

func (ix *IndexExpression) expressionNode()      {}
func (ix *IndexExpression) TokenLiteral() string { return ix.Tok.Literal }
func (ix *IndexExpression) String() string {
	return "(" + ix.Left.String() + "[" + ix.Index.String() + "])"
}

// ArrayLiteral is [elem, elem, ...].
type ArrayLiteral struct {
	Tok      Token
	Elements []Expression
}

func (al *ArrayLiteral) expressionNode()      {}
func (al *ArrayLiteral) TokenLiteral() string { return al.Tok.Literal }
func (al *ArrayLiteral) String() string {
	elems := make([]string, len(al.Elements))
	for i, e := range al.Elements {
		elems[i] = e.String()
	}
	return "[" + strings.Join(elems, ", ") + "]"
}

// HashPair holds one key:value in a hash literal.
type HashPair struct {
	Key   Expression
	Value Expression
}

// HashLiteral is {key: value, ...}.
type HashLiteral struct {
	Tok   Token
	Pairs []HashPair
}

func (hl *HashLiteral) expressionNode()      {}
func (hl *HashLiteral) TokenLiteral() string { return hl.Tok.Literal }
func (hl *HashLiteral) String() string {
	pairs := make([]string, len(hl.Pairs))
	for i, p := range hl.Pairs {
		pairs[i] = p.Key.String() + ": " + p.Value.String()
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// --- Statements --------------------------------------------------------------

// LetStatement is let name = value.
type LetStatement struct {
	Tok   Token
	Name  *Identifier
	Value Expression
}

func (ls *LetStatement) statementNode()       {}
func (ls *LetStatement) TokenLiteral() string { return ls.Tok.Literal }
func (ls *LetStatement) String() string {
	return "let " + ls.Name.String() + " = " + ls.Value.String()
}

// ReturnStatement is return value.
type ReturnStatement struct {
	Tok   Token
	Value Expression
}

func (rs *ReturnStatement) statementNode()       {}
func (rs *ReturnStatement) TokenLiteral() string { return rs.Tok.Literal }
func (rs *ReturnStatement) String() string {
	if rs.Value == nil {
		return "return"
	}
	return "return " + rs.Value.String()
}

// ExpressionStatement is a bare expression used as a statement.
type ExpressionStatement struct {
	Tok        Token
	Expression Expression
}

func (es *ExpressionStatement) statementNode()       {}
func (es *ExpressionStatement) TokenLiteral() string { return es.Tok.Literal }
func (es *ExpressionStatement) String() string {
	if es.Expression != nil {
		return es.Expression.String()
	}
	return ""
}

// BlockStatement is { stmt; stmt; ... }.
type BlockStatement struct {
	Tok        Token
	Statements []Statement
}

func (bs *BlockStatement) statementNode()       {}
func (bs *BlockStatement) TokenLiteral() string { return bs.Tok.Literal }
func (bs *BlockStatement) String() string {
	var sb strings.Builder
	sb.WriteString("{ ")
	for _, s := range bs.Statements {
		sb.WriteString(s.String())
		sb.WriteString("; ")
	}
	sb.WriteString("}")
	return sb.String()
}
```

### The parser: one table, one loop

This is the heart of the exercise, and almost all of it exists to support the few lines in `parseExpression`. The `infixBP` map is the only place precedence is written down: every infix operator maps to a binding power, and `precLowest = 0` is the threshold passed at the top level to mean "consume everything." `parseExpression(minBP)` calls the current token's prefix function to get a left-hand side, then loops: while the next token has an infix binding power strictly greater than `minBP`, it advances onto that operator and calls its infix function, which folds the existing left side into a larger node. When the next operator's power drops to `minBP` or below, the loop returns and leaves that operator for whichever enclosing call is waiting for it.

Associativity is two lines. `parseInfixExpression` reads the operator's own binding power and recurses with it unchanged for the left-associative operators — passing the operator's own power means a second operator at the same level is *not* greater than the threshold, so it is left for the outer loop and the tree leans left. For the one right-associative arithmetic operator, `**`, it subtracts one before recursing, so a second `**` *is* greater than the threshold and is swallowed by the recursion, leaning the tree right. Assignment is handled by its own `parseAssignExpression`, which recurses with `precAssign - 1` for the same reason. Note that `(` and `[` each appear in two roles: `parseGroupedExpression` (prefix) versus `parseCallExpression` (infix) for `(`, and `parseArrayLiteral` (prefix) versus `parseIndexExpression` (infix) for `[`. The parser never has to disambiguate them explicitly — position does it, because the prefix table is consulted to *start* an expression and the infix table to *extend* one.

Create `parser.go`:

```go
// Package parser implements a Pratt (top-down operator precedence) parser
// for the Monkey language. The package is self-contained: it defines its own
// lexer, AST, and parse functions. A caller calls New with the source text,
// then Parse to obtain the root Program node.
package parser

import (
	"fmt"
	"strconv"
)

// --- Precedence levels -------------------------------------------------------

// Binding powers (precedence levels). Higher value = tighter binding.
// Every infix operator maps to a value here; the loop in parseExpression
// continues as long as the next operator's BP exceeds the current minimum.
const (
	precLowest  = iota // 0 — starting point, never assigned to an operator
	precAssign         // 1  =
	precOr             // 2  ||
	precAnd            // 3  &&
	precEquals         // 4  == !=
	precCompare        // 5  < > <= >=
	precRange          // 6  ..
	precSum            // 7  + -
	precProduct        // 8  * / %
	precPower          // 9  **  (right-associative)
	precPrefix         // 10 prefix operators: ! - ~  (used in prefix fns)
	precCall           // 11 f(args)
	precIndex          // 12 a[i]
)

// infixBP maps each token type to its left-binding power.
var infixBP = map[TokenType]int{
	ASSIGN:   precAssign,
	PIPEPIPE: precOr,
	AMPAMP:   precAnd,
	EQEQ:     precEquals,
	BANGEQ:   precEquals,
	LT:       precCompare,
	GT:       precCompare,
	LTEQ:     precCompare,
	GTEQ:     precCompare,
	DOTDOT:   precRange,
	PLUS:     precSum,
	MINUS:    precSum,
	STAR:     precProduct,
	SLASH:    precProduct,
	PERCENT:  precProduct,
	STARSTAR: precPower,
	LPAREN:   precCall,
	LBRACKET: precIndex,
}

// --- Parse error -------------------------------------------------------------

// ParseError describes one syntax error with source location.
type ParseError struct {
	Line    int
	Col     int
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Message)
}

// --- Parser ------------------------------------------------------------------

type prefixParseFn func() Expression
type infixParseFn func(left Expression) Expression

// Parser consumes tokens from the Lexer and builds an AST.
type Parser struct {
	lex    *Lexer
	cur    Token
	peek   Token
	errors []*ParseError

	prefixFns map[TokenType]prefixParseFn
	infixFns  map[TokenType]infixParseFn
}

// New returns a Parser ready to parse src.
func New(src string) *Parser {
	p := &Parser{
		lex:       NewLexer(src),
		prefixFns: make(map[TokenType]prefixParseFn),
		infixFns:  make(map[TokenType]infixParseFn),
	}
	p.registerPrefixFns()
	p.registerInfixFns()
	// prime cur and peek
	p.advance()
	p.advance()
	return p
}

// Errors returns all parse errors collected during Parse.
func (p *Parser) Errors() []*ParseError {
	return p.errors
}

// Parse parses the entire program and returns the root node.
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

// --- Token navigation --------------------------------------------------------

func (p *Parser) advance() {
	p.cur = p.peek
	p.peek = p.lex.Next()
}

func (p *Parser) peekIs(tt TokenType) bool { return p.peek.Type == tt }

// expect advances and returns true when the peek token matches tt.
// On mismatch it records a parse error and returns false.
func (p *Parser) expect(tt TokenType) bool {
	if p.peekIs(tt) {
		p.advance()
		return true
	}
	p.errorf(p.peek.Line, p.peek.Col, "expected %s, got %s", tt, p.peek.Type)
	return false
}

func (p *Parser) errorf(line, col int, format string, args ...any) {
	p.errors = append(p.errors, &ParseError{
		Line:    line,
		Col:     col,
		Message: fmt.Sprintf(format, args...),
	})
}

// --- Registration ------------------------------------------------------------

func (p *Parser) registerPrefixFns() {
	p.prefixFns[IDENT] = p.parseIdentifier
	p.prefixFns[INT] = p.parseIntegerLiteral
	p.prefixFns[STRING] = p.parseStringLiteral
	p.prefixFns[TRUE] = p.parseBoolLiteral
	p.prefixFns[FALSE] = p.parseBoolLiteral
	p.prefixFns[NIL] = p.parseNilLiteral
	p.prefixFns[BANG] = p.parsePrefixExpression
	p.prefixFns[MINUS] = p.parsePrefixExpression
	p.prefixFns[TILDE] = p.parsePrefixExpression
	p.prefixFns[LPAREN] = p.parseGroupedExpression
	p.prefixFns[IF] = p.parseIfExpression
	p.prefixFns[FN] = p.parseFnLiteral
	p.prefixFns[LBRACKET] = p.parseArrayLiteral
	p.prefixFns[LBRACE] = p.parseHashLiteral
}

func (p *Parser) registerInfixFns() {
	arithmetic := []TokenType{PLUS, MINUS, STAR, SLASH, PERCENT, STARSTAR,
		EQEQ, BANGEQ, LT, GT, LTEQ, GTEQ, AMPAMP, PIPEPIPE, DOTDOT}
	for _, tt := range arithmetic {
		p.infixFns[tt] = p.parseInfixExpression
	}
	p.infixFns[ASSIGN] = p.parseAssignExpression
	p.infixFns[LPAREN] = p.parseCallExpression
	p.infixFns[LBRACKET] = p.parseIndexExpression
}

// --- Core Pratt loop ---------------------------------------------------------

// parseExpression is the heart of the Pratt algorithm.
//
// minBP is the minimum binding power the next infix operator must have for
// this call to consume it. Starting at precLowest means "consume everything";
// passing a higher value stops at weaker operators.
//
// Left-associative operators pass their own BP as the new minimum, so a second
// same-precedence operator is NOT consumed by the recursive call, creating
// left association.
//
// Right-associative operators pass BP-1, so the recursive call DOES consume a
// same-precedence operator, creating right association.
func (p *Parser) parseExpression(minBP int) Expression {
	prefix, ok := p.prefixFns[p.cur.Type]
	if !ok {
		p.errorf(p.cur.Line, p.cur.Col, "unexpected token %s", p.cur.Type)
		return nil
	}
	left := prefix()

	for {
		bp, hasInfix := infixBP[p.peek.Type]
		if !hasInfix || bp <= minBP {
			break
		}
		infix := p.infixFns[p.peek.Type]
		p.advance()
		left = infix(left)
	}
	return left
}

// --- Prefix parse functions --------------------------------------------------

func (p *Parser) parseIdentifier() Expression {
	return &Identifier{Tok: p.cur, Name: p.cur.Literal}
}

func (p *Parser) parseIntegerLiteral() Expression {
	v, err := strconv.ParseInt(p.cur.Literal, 10, 64)
	if err != nil {
		p.errorf(p.cur.Line, p.cur.Col, "cannot parse %q as integer", p.cur.Literal)
		return nil
	}
	return &IntegerLiteral{Tok: p.cur, Value: v}
}

func (p *Parser) parseStringLiteral() Expression {
	return &StringLiteral{Tok: p.cur, Value: p.cur.Literal}
}

func (p *Parser) parseBoolLiteral() Expression {
	return &BoolLiteral{Tok: p.cur, Value: p.cur.Type == TRUE}
}

func (p *Parser) parseNilLiteral() Expression {
	return &NilLiteral{Tok: p.cur}
}

func (p *Parser) parsePrefixExpression() Expression {
	tok := p.cur
	op := p.cur.Literal
	p.advance()
	right := p.parseExpression(precPrefix)
	return &PrefixExpression{Tok: tok, Operator: op, Right: right}
}

// parseGroupedExpression handles (expr) — groups, not call args.
func (p *Parser) parseGroupedExpression() Expression {
	p.advance() // consume (
	expr := p.parseExpression(precLowest)
	if !p.expect(RPAREN) {
		return nil
	}
	return expr
}

// parseIfExpression handles if (cond) { consequence } else { alternative }.
func (p *Parser) parseIfExpression() Expression {
	tok := p.cur
	if !p.expect(LPAREN) {
		return nil
	}
	p.advance()
	cond := p.parseExpression(precLowest)
	if !p.expect(RPAREN) {
		return nil
	}
	if !p.expect(LBRACE) {
		return nil
	}
	cons := p.parseBlockStatement()

	var alt *BlockStatement
	if p.peekIs(ELSE) {
		p.advance()
		if !p.expect(LBRACE) {
			return nil
		}
		alt = p.parseBlockStatement()
	}
	return &IfExpression{Tok: tok, Condition: cond, Consequence: cons, Alternative: alt}
}

// parseFnLiteral handles fn(params) { body }.
func (p *Parser) parseFnLiteral() Expression {
	tok := p.cur
	if !p.expect(LPAREN) {
		return nil
	}
	params := p.parseFnParams()
	if !p.expect(LBRACE) {
		return nil
	}
	body := p.parseBlockStatement()
	return &FnLiteral{Tok: tok, Params: params, Body: body}
}

func (p *Parser) parseFnParams() []*Identifier {
	if p.peekIs(RPAREN) {
		p.advance()
		return nil
	}
	p.advance()
	params := []*Identifier{{Tok: p.cur, Name: p.cur.Literal}}
	for p.peekIs(COMMA) {
		p.advance()
		p.advance()
		params = append(params, &Identifier{Tok: p.cur, Name: p.cur.Literal})
	}
	if !p.expect(RPAREN) {
		return nil
	}
	return params
}

// parseArrayLiteral handles [elem, elem, ...].
func (p *Parser) parseArrayLiteral() Expression {
	tok := p.cur
	elems := p.parseExpressionList(RBRACKET)
	return &ArrayLiteral{Tok: tok, Elements: elems}
}

// parseHashLiteral handles {key: value, ...}.
// Note: a bare { at the statement level is a hash literal, not a block.
// Blocks are only introduced by if/fn keywords.
func (p *Parser) parseHashLiteral() Expression {
	tok := p.cur
	var pairs []HashPair
	for !p.peekIs(RBRACE) && !p.peekIs(EOF) {
		p.advance()
		key := p.parseExpression(precLowest)
		if !p.expect(COLON) {
			return nil
		}
		p.advance()
		val := p.parseExpression(precLowest)
		pairs = append(pairs, HashPair{Key: key, Value: val})
		if !p.peekIs(RBRACE) && !p.expect(COMMA) {
			return nil
		}
	}
	if !p.expect(RBRACE) {
		return nil
	}
	return &HashLiteral{Tok: tok, Pairs: pairs}
}

// --- Infix parse functions ---------------------------------------------------

// parseInfixExpression handles left-associative binary operators.
// It passes its own BP as the minimum, so a second same-precedence operator
// is not consumed by the recursive call — this creates left association.
func (p *Parser) parseInfixExpression(left Expression) Expression {
	tok := p.cur
	op := p.cur.Literal
	bp := infixBP[p.cur.Type]

	// Right-associative operators: ** passes bp-1 so that a second **
	// IS consumed by the recursive call, producing right association.
	if p.cur.Type == STARSTAR {
		bp--
	}
	p.advance()
	right := p.parseExpression(bp)
	return &InfixExpression{Tok: tok, Left: left, Operator: op, Right: right}
}

// parseAssignExpression handles right-associative assignment: a = b = c
// parses as a = (b = c).
func (p *Parser) parseAssignExpression(left Expression) Expression {
	tok := p.cur
	p.advance()
	// pass precAssign-1 so that another = on the right IS consumed
	right := p.parseExpression(precAssign - 1)
	return &InfixExpression{Tok: tok, Left: left, Operator: "=", Right: right}
}

// parseCallExpression handles callee(args...). The left side is the callee.
func (p *Parser) parseCallExpression(callee Expression) Expression {
	tok := p.cur
	args := p.parseExpressionList(RPAREN)
	return &CallExpression{Tok: tok, Callee: callee, Args: args}
}

// parseIndexExpression handles expr[index].
func (p *Parser) parseIndexExpression(left Expression) Expression {
	tok := p.cur
	p.advance()
	index := p.parseExpression(precLowest)
	if !p.expect(RBRACKET) {
		return nil
	}
	return &IndexExpression{Tok: tok, Left: left, Index: index}
}

// parseExpressionList parses a comma-separated list terminated by end.
func (p *Parser) parseExpressionList(end TokenType) []Expression {
	if p.peekIs(end) {
		p.advance()
		return nil
	}
	p.advance()
	exprs := []Expression{p.parseExpression(precLowest)}
	for p.peekIs(COMMA) {
		p.advance()
		p.advance()
		exprs = append(exprs, p.parseExpression(precLowest))
	}
	if !p.expect(end) {
		return nil
	}
	return exprs
}

// --- Statement parsing -------------------------------------------------------

func (p *Parser) parseStatement() Statement {
	switch p.cur.Type {
	case LET, CONST:
		return p.parseLetStatement()
	case RETURN:
		return p.parseReturnStatement()
	default:
		return p.parseExpressionStatement()
	}
}

func (p *Parser) parseLetStatement() Statement {
	tok := p.cur
	if !p.expect(IDENT) {
		return nil
	}
	name := &Identifier{Tok: p.cur, Name: p.cur.Literal}
	if !p.expect(ASSIGN) {
		return nil
	}
	p.advance()
	val := p.parseExpression(precLowest)
	if p.peekIs(SEMICOLON) {
		p.advance()
	}
	return &LetStatement{Tok: tok, Name: name, Value: val}
}

func (p *Parser) parseReturnStatement() Statement {
	tok := p.cur
	if p.peekIs(SEMICOLON) || p.peekIs(RBRACE) || p.peekIs(EOF) {
		return &ReturnStatement{Tok: tok}
	}
	p.advance()
	val := p.parseExpression(precLowest)
	if p.peekIs(SEMICOLON) {
		p.advance()
	}
	return &ReturnStatement{Tok: tok, Value: val}
}

func (p *Parser) parseExpressionStatement() Statement {
	tok := p.cur
	expr := p.parseExpression(precLowest)
	if p.peekIs(SEMICOLON) {
		p.advance()
	}
	return &ExpressionStatement{Tok: tok, Expression: expr}
}

func (p *Parser) parseBlockStatement() *BlockStatement {
	tok := p.cur // the opening {
	bs := &BlockStatement{Tok: tok}
	p.advance()
	for p.cur.Type != RBRACE && p.cur.Type != EOF {
		if s := p.parseStatement(); s != nil {
			bs.Statements = append(bs.Statements, s)
		}
		p.advance()
	}
	return bs
}
```

### The runnable demo

A test pins one property at a time; the demo shows the whole engine working on a realistic fragment. It parses a small Monkey program with a function literal, a call whose arguments are themselves expressions, and an `if`/`else`, then prints the program's `String()`. Because every operator stringifies fully parenthesized, the output is the precedence tree made visible: `3 * 4` and `2 ** 8` appear wrapped, and `result > 100` appears as a parenthesized comparison.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	parser "example.com/prattparser"
)

func main() {
	src := `
let add = fn(x, y) { x + y };
let result = add(3 * 4, 2 ** 8);
if (result > 100) {
	return result;
} else {
	return 0;
}
`
	p := parser.New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "error: %s\n", e.Error())
		}
		os.Exit(1)
	}
	fmt.Println(prog.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
let add = fn(x, y) { (x + y); }
let result = add((3 * 4), (2 ** 8))
if ((result > 100)) { return result; } else { return 0; }
```

### Tests

The tests pin behaviour as strings, which is the right granularity for a parser: a precedence or associativity bug changes the parenthesization and the assertion fails with a readable diff. `TestPrecedence` walks a table of mixed-operator expressions and checks the exact tree shape; `TestRightAssociativity` isolates the two right-associative operators, `**` and `=`; `TestPrefixExpressions` covers unary operators and grouping; `TestCallExpressions` and `TestIndexExpressions` exercise the dual-role `(` and `[`; the statement tests confirm `let`, `return`, `if`, `fn`, and the collection literals. The two `Example` functions carry `// Output:` comments that `go test` verifies automatically, so they fail the moment the parenthesization drifts.

Create `parser_test.go`:

```go
package parser

import (
	"fmt"
	"testing"
)

// parseExpr is a test helper: parse a single expression and return its String().
func parseExpr(src string) (string, []*ParseError) {
	p := New(src)
	prog := p.Parse()
	if len(prog.Statements) == 0 {
		return "", p.errors
	}
	es, ok := prog.Statements[0].(*ExpressionStatement)
	if !ok {
		return prog.Statements[0].String(), p.errors
	}
	if es.Expression == nil {
		return "", p.errors
	}
	return es.Expression.String(), p.errors
}

// TestPrecedence verifies that the parser respects operator precedence.
func TestPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want string
	}{
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"1 * 2 + 3", "((1 * 2) + 3)"},
		{"2 ** 3 ** 2", "(2 ** (3 ** 2))"}, // right-associative
		{"a == b != c", "((a == b) != c)"}, // left-associative
		{"a && b || c", "((a && b) || c)"},
		{"a || b && c", "(a || (b && c))"},
		{"a + b * c - d", "((a + (b * c)) - d)"},
		{"!a == true", "((!a) == true)"},
		{"-a * b", "((-a) * b)"},
		{"a < b == c > d", "((a < b) == (c > d))"},
	}

	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			got, errs := parseExpr(tc.src)
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRightAssociativity checks assignment and exponentiation.
func TestRightAssociativity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want string
	}{
		{"a = b = c", "(a = (b = c))"},
		{"2 ** 3 ** 2", "(2 ** (3 ** 2))"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			got, errs := parseExpr(tc.src)
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrefixExpressions checks prefix operators and grouped expressions.
func TestPrefixExpressions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want string
	}{
		{"!true", "(!true)"},
		{"-42", "(-42)"},
		{"~x", "(~x)"},
		{"!(a == b)", "(!(a == b))"},
		{"(1 + 2) * 3", "((1 + 2) * 3)"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			got, errs := parseExpr(tc.src)
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCallExpressions verifies zero, one, and multiple arguments.
func TestCallExpressions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want string
	}{
		{"f()", "f()"},
		{"f(1)", "f(1)"},
		{"f(1, 2, 3)", "f(1, 2, 3)"},
		{"f(g(x))", "f(g(x))"},
		{"add(a + b, c * d)", "add((a + b), (c * d))"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			got, errs := parseExpr(tc.src)
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIndexExpressions checks array subscripting including chained access.
func TestIndexExpressions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want string
	}{
		{"a[0]", "(a[0])"},
		{"a[1 + 2]", "(a[(1 + 2)])"},
		{"a[0][1]", "((a[0])[1])"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			got, errs := parseExpr(tc.src)
			if len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLetStatement verifies let statement parsing.
func TestLetStatement(t *testing.T) {
	t.Parallel()

	p := New("let x = 1 + 2;")
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(prog.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(prog.Statements))
	}
	ls, ok := prog.Statements[0].(*LetStatement)
	if !ok {
		t.Fatalf("expected *LetStatement, got %T", prog.Statements[0])
	}
	if ls.Name.Name != "x" {
		t.Errorf("name = %q, want %q", ls.Name.Name, "x")
	}
	if got := ls.Value.String(); got != "(1 + 2)" {
		t.Errorf("value = %q, want %q", got, "(1 + 2)")
	}
}

// TestReturnStatement verifies that a return statement keeps its value
// expression and parses it with the same precedence rules.
func TestReturnStatement(t *testing.T) {
	t.Parallel()

	p := New("return x + 1;")
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	rs, ok := prog.Statements[0].(*ReturnStatement)
	if !ok {
		t.Fatalf("expected *ReturnStatement, got %T", prog.Statements[0])
	}
	if got := rs.Value.String(); got != "(x + 1)" {
		t.Errorf("value = %q, want %q", got, "(x + 1)")
	}
}

// TestIfExpression verifies if / if-else expressions.
func TestIfExpression(t *testing.T) {
	t.Parallel()

	src := "if (x > 0) { return x; } else { return 0; }"
	p := New(src)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	if len(prog.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(prog.Statements))
	}
	es, ok := prog.Statements[0].(*ExpressionStatement)
	if !ok {
		t.Fatalf("expected *ExpressionStatement, got %T", prog.Statements[0])
	}
	ife, ok := es.Expression.(*IfExpression)
	if !ok {
		t.Fatalf("expected *IfExpression, got %T", es.Expression)
	}
	if ife.Alternative == nil {
		t.Fatal("expected an else branch")
	}
}

// TestFnLiteral checks function literal parsing and parameter count.
func TestFnLiteral(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src        string
		paramCount int
	}{
		{"fn() { 0 }", 0},
		{"fn(x) { x }", 1},
		{"fn(x, y) { x + y }", 2},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			p := New(tc.src)
			prog := p.Parse()
			if errs := p.Errors(); len(errs) > 0 {
				t.Fatalf("parse errors: %v", errs)
			}
			es := prog.Statements[0].(*ExpressionStatement)
			fl := es.Expression.(*FnLiteral)
			if len(fl.Params) != tc.paramCount {
				t.Errorf("param count = %d, want %d", len(fl.Params), tc.paramCount)
			}
		})
	}
}

// TestArrayAndHashLiterals checks collection literal parsing.
func TestArrayAndHashLiterals(t *testing.T) {
	t.Parallel()

	p := New(`[1, 2, 3]`)
	prog := p.Parse()
	if errs := p.Errors(); len(errs) > 0 {
		t.Fatalf("array parse errors: %v", errs)
	}
	es := prog.Statements[0].(*ExpressionStatement)
	al, ok := es.Expression.(*ArrayLiteral)
	if !ok {
		t.Fatalf("expected *ArrayLiteral, got %T", es.Expression)
	}
	if len(al.Elements) != 3 {
		t.Errorf("element count = %d, want 3", len(al.Elements))
	}

	p2 := New(`{"a": 1, "b": 2}`)
	prog2 := p2.Parse()
	if errs := p2.Errors(); len(errs) > 0 {
		t.Fatalf("hash parse errors: %v", errs)
	}
	es2 := prog2.Statements[0].(*ExpressionStatement)
	hl, ok := es2.Expression.(*HashLiteral)
	if !ok {
		t.Fatalf("expected *HashLiteral, got %T", es2.Expression)
	}
	if len(hl.Pairs) != 2 {
		t.Errorf("pair count = %d, want 2", len(hl.Pairs))
	}
}

// ExampleNew demonstrates the core Pratt behaviour: * binds tighter than +,
// so 1 + 2 * 3 produces (1 + (2 * 3)).
func ExampleNew() {
	p := New("1 + 2 * 3")
	prog := p.Parse()
	fmt.Println(prog.String())
	// Output: (1 + (2 * 3))
}

// ExampleNew_rightAssociative shows that ** is right-associative:
// 2 ** 3 ** 2 parses as 2 ** (3 ** 2), not (2 ** 3) ** 2.
func ExampleNew_rightAssociative() {
	p := New("2 ** 3 ** 2")
	prog := p.Parse()
	fmt.Println(prog.String())
	// Output: (2 ** (3 ** 2))
}
```

## Review

The parser is correct when the parenthesized string matches the intended tree for every shape: `*` inside `+`, `**` nesting to the right, `=` nesting to the right, and the comparison and logical operators sitting at the levels `infixBP` assigns them. The single source of truth for all of that is the `infixBP` table plus the `minBP` comparison in `parseExpression`; if a precedence is wrong, the fix is one number in the table, not a restructured function. Confirm that the two right-associative cases recurse with a threshold one below their own power while every left-associative case recurses with its own power, that `(` and `[` each work in both their prefix and infix roles, and that a leading `{` at statement level reaches `parseHashLiteral` rather than being misread as a block. The whole suite plus the demo passing under `go test -race ./...` establishes those properties.

The mistakes worth re-reading from the concepts file apply directly here. Recursing with a higher threshold instead of `bp - 1` flips associativity the wrong way. Forgetting `p.advance()` before recursing in an infix function makes `parseExpression` look for a prefix function for the operator and report a spurious "unexpected token." Priming `cur` and `peek` only once in `New` leaves `cur` holding a zero-value `EOF` token, so `Parse` returns an empty program; the two `p.advance()` calls in `New` are mandatory.

## Resources

- [Simple but Powerful Pratt Parsing (matklad, 2020)](https://matklad.github.io/2020/04/13/simple-but-powerful-pratt-parsing.html) — the clearest modern derivation of binding power and the `± 1` associativity trick.
- [Top Down Operator Precedence (Douglas Crockford, 2007)](https://crockford.com/javascript/tdop/tdop.html) — Crockford's reworking of Pratt's original technique into the prefix/infix (`nud`/`led`) function pair.
- [Pratt Parsers: Expression Parsing Made Easy (Bob Nystrom, 2011)](https://journal.stuffwithstuff.com/2011/03/19/pratt-parsers-expression-parsing-made-easy/) — a worked Java implementation with the same prefix/infix table structure.
- [Writing An Interpreter In Go — Chapter 2: Parsing (Thorsten Ball)](https://interpreterbook.com/) — the Monkey parser this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-error-recovery-and-multiple-errors.md](02-error-recovery-and-multiple-errors.md)
