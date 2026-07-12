# Exercise 1: Core SQL Parser

This exercise builds the complete core parser: a typed AST, a Pratt expression engine, and recursive-descent clause parsers for `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `CREATE TABLE`, `DROP TABLE`, and the transaction-control statements. The hard parts — operator precedence, the `BETWEEN ... AND` separator clash, the `IS NULL` / `[NOT] IN` / `[NOT] LIKE` / `[NOT] BETWEEN` special forms, and editor-grade error locations — all live here.

The module is fully self-contained. It bundles its own minimal SQL lexer in a `lexer` subpackage and depends on nothing but the standard library. Nothing here imports another exercise.

## What you'll build

```text
sqlparser/
  go.mod
  lexer/
    lexer.go          minimal SQL lexer: tokens + Lexer + New + NextToken
  ast.go              AST node types and their String() SQL regenerators
  parser.go           Parser, error types, Pratt engine, statement parsers
  parser_test.go      table-driven tests for every statement and predicate
  cmd/
    demo/
      main.go         parse a multi-statement script and reprint each AST
```

- Files: `lexer/lexer.go`, `ast.go`, `parser.go`, `parser_test.go`, `cmd/demo/main.go`.
- Implement: the AST interfaces (`Statement`, `Expression`) and nodes, `New`, `ParseStatement`, `ParseAll`, `Errors`, the Pratt core (`parseExpression`, `parseInfix`, `parsePrefix`, `infixBP`), the special-form parsers (`parseBetween`, `parseIn`, `parseLike`), and `ErrSyntax` / `ErrUnexpectedEOF` / `ParseError`.
- Test: every statement type, operator precedence, the four special predicates, round-trip stability, and that syntax errors satisfy `errors.Is(err, ErrSyntax)`.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/05-sql-parser/01-core-sql-parser/lexer go-solutions/39-capstone-database-engine/05-sql-parser/01-core-sql-parser/cmd/demo && cd go-solutions/39-capstone-database-engine/05-sql-parser/01-core-sql-parser
```

### The lexer boundary

The parser consumes a flat token stream and never touches raw bytes. Each token carries a type, its literal text (keywords canonicalized to uppercase), and a 1-based line and column for error reporting. The lexer is a pure function from source to tokens: every grammatical decision — whether `LEFT` starts a join or names a column, whether `(` opens a subquery or a grouped expression — is pushed up to the parser, which is where the two-token lookahead lives. This exercise bundles a compact lexer sufficient for the grammar; a full-featured SQL lexer (nested comments, bind parameters, dollar-quoting) is a separate concern.

### The Pratt engine, split at the nud/led seam

`parseExpression(minBP)` does two things: it parses one prefix (the null-denotation — a literal, a column, a parenthesized group, a unary operator, a function call) and then runs the left-denotation loop in `parseInfix`, which absorbs infix operators and the special SQL forms whose binding power exceeds `minBP`. Splitting the loop out of `parseExpression` is deliberate: the select-item parser must inspect a qualified column for a trailing `*` before it knows whether to continue the expression, and it re-enters the operator loop with `parseInfix(col, 0)` so that `SELECT t.col + 1 FROM t` keeps the `+ 1`. The same seam is what later exercises reuse to extend the grammar without copying the operator loop.

The binding-power table is encoded once in `infixBP`. The special forms (`IS`, `BETWEEN`, `IN`, `LIKE`, and the `NOT`-prefixed variants) are handled by dedicated functions inside `parseInfix`, all guarded by `cmpBP > minBP` so they bind at comparison level and never get pulled into the right side of an arithmetic operator. The BETWEEN bounds are parsed at `minBP=cmpBP`, which is the single trick that keeps the `AND` separator from being swallowed as a logical conjunction.

### Errors that point at a token

A `ParseError` records the line and column of the offending token and wraps `ErrSyntax`. The parser accumulates errors in a slice and returns the first, so one mistake does not cascade into a flood. `ParseStatement` returns `ErrUnexpectedEOF` at clean end of input, which is how `ParseAll` knows to stop. Because the error wraps a sentinel, a caller writes `errors.Is(err, ErrSyntax)` rather than matching on message text, and `errors.As(err, &pe)` recovers the location.

Create `lexer/lexer.go`:

```go
package lexer

import (
	"fmt"
	"strings"
)

// TokenType identifies the category of a SQL token.
type TokenType int

const (
	TokenError TokenType = iota
	TokenEOF

	// Literals.
	TokenIdent  // unquoted identifier
	TokenQIdent // double-quoted identifier
	TokenInt    // integer literal: 42
	TokenFloat  // float literal: 3.14, 1.5e10
	TokenString // single-quoted string literal

	// Keywords.
	TokenSelect
	TokenFrom
	TokenWhere
	TokenInsert
	TokenInto
	TokenValues
	TokenUpdate
	TokenSet
	TokenDelete
	TokenCreate
	TokenTable
	TokenDrop
	TokenIndex
	TokenOn
	TokenAnd
	TokenOr
	TokenNot
	TokenNull
	TokenTrue
	TokenFalse
	TokenOrder
	TokenBy
	TokenAsc
	TokenDesc
	TokenLimit
	TokenOffset
	TokenJoin
	TokenLeft
	TokenRight
	TokenInner
	TokenOuter
	TokenGroup
	TokenHaving
	TokenAs
	TokenDistinct
	TokenCount
	TokenSum
	TokenAvg
	TokenMin
	TokenMax
	TokenIn
	TokenBetween
	TokenLike
	TokenIs
	TokenExists
	TokenPrimary
	TokenKey
	TokenInteger
	TokenText
	TokenReal
	TokenBoolean
	TokenBegin
	TokenCommit
	TokenRollback

	// Operators.
	TokenPlus     // +
	TokenMinus    // -
	TokenAsterisk // *
	TokenSlash    // /
	TokenEq       // =
	TokenNeq      // != or <>
	TokenLt       // <
	TokenGt       // >
	TokenLtEq     // <=
	TokenGtEq     // >=

	// Punctuation.
	TokenLParen    // (
	TokenRParen    // )
	TokenComma     // ,
	TokenSemicolon // ;
	TokenDot       // .
)

// Token is a single lexical unit in a SQL source string. Line and Col are
// 1-based so the parser can report editor-friendly positions.
type Token struct {
	Type    TokenType
	Literal string
	Pos     int
	Line    int
	Col     int
}

// String returns a debug representation.
func (tok Token) String() string {
	return fmt.Sprintf("Token(%d, %q, %d:%d)", tok.Type, tok.Literal, tok.Line, tok.Col)
}

var keywords = map[string]TokenType{
	"SELECT": TokenSelect, "FROM": TokenFrom, "WHERE": TokenWhere,
	"INSERT": TokenInsert, "INTO": TokenInto, "VALUES": TokenValues,
	"UPDATE": TokenUpdate, "SET": TokenSet, "DELETE": TokenDelete,
	"CREATE": TokenCreate, "TABLE": TokenTable, "DROP": TokenDrop,
	"INDEX": TokenIndex, "ON": TokenOn, "AND": TokenAnd, "OR": TokenOr,
	"NOT": TokenNot, "NULL": TokenNull, "TRUE": TokenTrue, "FALSE": TokenFalse,
	"ORDER": TokenOrder, "BY": TokenBy, "ASC": TokenAsc, "DESC": TokenDesc,
	"LIMIT": TokenLimit, "OFFSET": TokenOffset, "JOIN": TokenJoin,
	"LEFT": TokenLeft, "RIGHT": TokenRight, "INNER": TokenInner,
	"OUTER": TokenOuter, "GROUP": TokenGroup, "HAVING": TokenHaving,
	"AS": TokenAs, "DISTINCT": TokenDistinct, "COUNT": TokenCount,
	"SUM": TokenSum, "AVG": TokenAvg, "MIN": TokenMin, "MAX": TokenMax,
	"IN": TokenIn, "BETWEEN": TokenBetween, "LIKE": TokenLike, "IS": TokenIs,
	"EXISTS": TokenExists, "PRIMARY": TokenPrimary, "KEY": TokenKey,
	"INTEGER": TokenInteger, "TEXT": TokenText, "REAL": TokenReal,
	"BOOLEAN": TokenBoolean, "BEGIN": TokenBegin, "COMMIT": TokenCommit,
	"ROLLBACK": TokenRollback,
}

func lookupIdent(ident string) TokenType {
	if tt, ok := keywords[strings.ToUpper(ident)]; ok {
		return tt
	}
	return TokenIdent
}

// Lexer tokenizes a SQL source string. Create one with New; call NextToken
// until it returns TokenEOF or TokenError.
type Lexer struct {
	input   string
	pos     int  // byte index of ch
	readPos int  // byte index of the next byte to consume
	ch      byte // the byte at pos; 0 at EOF
	line    int
	col     int
}

// New creates a Lexer for input and reads the first byte.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1, col: 0}
	l.readChar()
	return l
}

func (l *Lexer) readChar() {
	if l.readPos >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPos]
	}
	l.pos = l.readPos
	l.readPos++
	if l.ch == '\n' {
		l.line++
		l.col = 0
	} else {
		l.col++
	}
}

func (l *Lexer) peekChar() byte {
	if l.readPos >= len(l.input) {
		return 0
	}
	return l.input[l.readPos]
}

func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\r' || l.ch == '\n' {
		l.readChar()
	}
}

// NextToken returns the next token in the SQL source. Comments are skipped.
func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	startPos, startLine, startCol := l.pos, l.line, l.col

	switch {
	case l.ch == 0:
		return Token{Type: TokenEOF, Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '-' && l.peekChar() == '-':
		l.skipLineComment()
		return l.NextToken()

	case l.ch == '/' && l.peekChar() == '*':
		if err := l.skipBlockComment(); err != nil {
			return Token{Type: TokenError, Literal: err.Error(), Pos: startPos, Line: startLine, Col: startCol}
		}
		return l.NextToken()

	case l.ch == '\'':
		return l.readString(startPos, startLine, startCol)

	case l.ch == '"':
		return l.readQuotedIdent(startPos, startLine, startCol)

	case isLetter(l.ch):
		return l.readIdentOrKeyword(startPos, startLine, startCol)

	case isDigit(l.ch):
		return l.readNumber(startPos, startLine, startCol)

	case l.ch == '<' && l.peekChar() == '>':
		l.readChar()
		l.readChar()
		return Token{Type: TokenNeq, Literal: "<>", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '<' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenLtEq, Literal: "<=", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '>' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenGtEq, Literal: ">=", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '!' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenNeq, Literal: "!=", Pos: startPos, Line: startLine, Col: startCol}

	default:
		tok := l.readSingle(startPos, startLine, startCol)
		l.readChar()
		return tok
	}
}

func (l *Lexer) readSingle(pos, line, col int) Token {
	var tt TokenType
	switch l.ch {
	case '+':
		tt = TokenPlus
	case '-':
		tt = TokenMinus
	case '*':
		tt = TokenAsterisk
	case '/':
		tt = TokenSlash
	case '=':
		tt = TokenEq
	case '<':
		tt = TokenLt
	case '>':
		tt = TokenGt
	case '(':
		tt = TokenLParen
	case ')':
		tt = TokenRParen
	case ',':
		tt = TokenComma
	case ';':
		tt = TokenSemicolon
	case '.':
		tt = TokenDot
	default:
		return Token{
			Type:    TokenError,
			Literal: fmt.Sprintf("unexpected character %q at %d:%d", l.ch, line, col),
			Pos:     pos, Line: line, Col: col,
		}
	}
	return Token{Type: tt, Literal: string(l.ch), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) skipLineComment() {
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
}

func (l *Lexer) skipBlockComment() error {
	startLine, startCol := l.line, l.col
	l.readChar() // consume /
	l.readChar() // consume *
	depth := 1
	for depth > 0 {
		if l.ch == 0 {
			return fmt.Errorf("unterminated block comment starting at %d:%d", startLine, startCol)
		}
		if l.ch == '/' && l.peekChar() == '*' {
			depth++
			l.readChar()
			l.readChar()
			continue
		}
		if l.ch == '*' && l.peekChar() == '/' {
			depth--
			l.readChar()
			l.readChar()
			continue
		}
		l.readChar()
	}
	return nil
}

func (l *Lexer) readString(pos, line, col int) Token {
	l.readChar() // consume opening '
	var buf strings.Builder
	for {
		if l.ch == 0 {
			return Token{Type: TokenError, Literal: fmt.Sprintf("unterminated string literal starting at %d:%d", line, col), Pos: pos, Line: line, Col: col}
		}
		if l.ch == '\'' {
			if l.peekChar() == '\'' {
				buf.WriteByte('\'')
				l.readChar()
				l.readChar()
				continue
			}
			l.readChar() // consume closing '
			break
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	return Token{Type: TokenString, Literal: buf.String(), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readQuotedIdent(pos, line, col int) Token {
	l.readChar() // consume opening "
	var buf strings.Builder
	for {
		if l.ch == 0 {
			return Token{Type: TokenError, Literal: fmt.Sprintf("unterminated quoted identifier starting at %d:%d", line, col), Pos: pos, Line: line, Col: col}
		}
		if l.ch == '"' {
			if l.peekChar() == '"' {
				buf.WriteByte('"')
				l.readChar()
				l.readChar()
				continue
			}
			l.readChar() // consume closing "
			break
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	return Token{Type: TokenQIdent, Literal: buf.String(), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readIdentOrKeyword(pos, line, col int) Token {
	start := l.pos
	for isLetter(l.ch) || isDigit(l.ch) || l.ch == '_' {
		l.readChar()
	}
	lit := l.input[start:l.pos]
	tt := lookupIdent(lit)
	if tt != TokenIdent {
		return Token{Type: tt, Literal: strings.ToUpper(lit), Pos: pos, Line: line, Col: col}
	}
	return Token{Type: TokenIdent, Literal: lit, Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readNumber(pos, line, col int) Token {
	start := l.pos
	tt := TokenInt
	for isDigit(l.ch) {
		l.readChar()
	}
	if l.ch == '.' && isDigit(l.peekChar()) {
		tt = TokenFloat
		l.readChar()
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	if l.ch == 'e' || l.ch == 'E' {
		tt = TokenFloat
		l.readChar()
		if l.ch == '+' || l.ch == '-' {
			l.readChar()
		}
		if !isDigit(l.ch) {
			return Token{Type: TokenError, Literal: fmt.Sprintf("malformed numeric literal at %d:%d", line, col), Pos: pos, Line: line, Col: col}
		}
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	return Token{Type: tt, Literal: l.input[start:l.pos], Pos: pos, Line: line, Col: col}
}

func isLetter(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}
```

The lexer keeps a two-offset cursor (`pos`, `readPos`) and a one-byte lookahead via `peekChar`. Keywords are lexed as identifiers first, then canonicalized to uppercase and resolved through a single map lookup, so the scanner loop never branches on keyword spelling. Comments are skipped (block comments nest via a depth counter). The numeric reader consumes a fractional dot only when a digit follows it, so `table.col` lexes as the identifier `table`, a `TokenDot`, and `col` rather than a malformed float.

Create `ast.go`:

```go
package parser

import (
	"fmt"
	"strings"
)

// Statement is the top-level interface for all SQL statements.
// stmtNode prevents accidental implementation by unrelated types.
type Statement interface {
	stmtNode()
	String() string
}

// Expression is the interface for all nodes in an expression tree.
type Expression interface {
	exprNode()
	String() string
}

// ColumnRef is a bare or qualified column name: [table.]column.
type ColumnRef struct {
	Table  string // empty for unqualified references
	Column string
}

func (*ColumnRef) exprNode() {}

func (c *ColumnRef) String() string {
	if c.Table != "" {
		return c.Table + "." + c.Column
	}
	return c.Column
}

// LiteralKind discriminates the kind of a scalar constant.
type LiteralKind int

const (
	LiteralInt LiteralKind = iota
	LiteralFloat
	LiteralString
	LiteralBool
	LiteralNull
)

// LiteralExpr is a scalar constant: integer, float, string, boolean, or NULL.
// Value holds the raw text from the source, except for NULL (Value is empty).
type LiteralExpr struct {
	Kind  LiteralKind
	Value string
}

func (*LiteralExpr) exprNode() {}

func (l *LiteralExpr) String() string {
	switch l.Kind {
	case LiteralString:
		return "'" + strings.ReplaceAll(l.Value, "'", "''") + "'"
	case LiteralNull:
		return "NULL"
	case LiteralBool:
		return strings.ToUpper(l.Value)
	default:
		return l.Value
	}
}

// BinaryExpr is a two-operand expression: left op right.
// String wraps with parentheses to make precedence explicit, enabling
// unambiguous round-trip tests.
type BinaryExpr struct {
	Left  Expression
	Op    string
	Right Expression
}

func (*BinaryExpr) exprNode() {}

func (b *BinaryExpr) String() string {
	return fmt.Sprintf("(%s %s %s)", b.Left.String(), b.Op, b.Right.String())
}

// UnaryExpr is a single-operand prefix expression: op operand.
type UnaryExpr struct {
	Op      string
	Operand Expression
}

func (*UnaryExpr) exprNode() {}

func (u *UnaryExpr) String() string {
	return fmt.Sprintf("(%s %s)", u.Op, u.Operand.String())
}

// FunctionCallExpr represents a SQL aggregate or scalar function call.
// Star is true for COUNT(*). Distinct is true for COUNT(DISTINCT expr).
type FunctionCallExpr struct {
	Name     string
	Args     []Expression
	Distinct bool
	Star     bool
}

func (*FunctionCallExpr) exprNode() {}

func (f *FunctionCallExpr) String() string {
	if f.Star {
		return f.Name + "(*)"
	}
	args := make([]string, len(f.Args))
	for i, a := range f.Args {
		args[i] = a.String()
	}
	if f.Distinct {
		return fmt.Sprintf("%s(DISTINCT %s)", f.Name, strings.Join(args, ", "))
	}
	return fmt.Sprintf("%s(%s)", f.Name, strings.Join(args, ", "))
}

// SubqueryExpr wraps a scalar subquery used in expression position.
type SubqueryExpr struct {
	Query *SelectStatement
}

func (*SubqueryExpr) exprNode() {}

func (s *SubqueryExpr) String() string {
	return "(" + s.Query.String() + ")"
}

// IsNullExpr is: expr IS [NOT] NULL.
type IsNullExpr struct {
	Expr  Expression
	IsNot bool
}

func (*IsNullExpr) exprNode() {}

func (i *IsNullExpr) String() string {
	if i.IsNot {
		return fmt.Sprintf("(%s IS NOT NULL)", i.Expr.String())
	}
	return fmt.Sprintf("(%s IS NULL)", i.Expr.String())
}

// BetweenExpr is: expr [NOT] BETWEEN lo AND hi.
type BetweenExpr struct {
	Expr Expression
	Not  bool
	Lo   Expression
	Hi   Expression
}

func (*BetweenExpr) exprNode() {}

func (b *BetweenExpr) String() string {
	not := ""
	if b.Not {
		not = "NOT "
	}
	return fmt.Sprintf("(%s %sBETWEEN %s AND %s)",
		b.Expr.String(), not, b.Lo.String(), b.Hi.String())
}

// InExpr is: expr [NOT] IN (values…) or expr [NOT] IN (subquery).
// Values and Subquery are mutually exclusive.
type InExpr struct {
	Expr     Expression
	Not      bool
	Values   []Expression
	Subquery *SelectStatement
}

func (*InExpr) exprNode() {}

func (i *InExpr) String() string {
	not := ""
	if i.Not {
		not = "NOT "
	}
	if i.Subquery != nil {
		return fmt.Sprintf("(%s %sIN (%s))", i.Expr.String(), not, i.Subquery.String())
	}
	vals := make([]string, len(i.Values))
	for j, v := range i.Values {
		vals[j] = v.String()
	}
	return fmt.Sprintf("(%s %sIN (%s))", i.Expr.String(), not, strings.Join(vals, ", "))
}

// LikeExpr is: expr [NOT] LIKE pattern.
type LikeExpr struct {
	Expr    Expression
	Not     bool
	Pattern Expression
}

func (*LikeExpr) exprNode() {}

func (l *LikeExpr) String() string {
	not := ""
	if l.Not {
		not = "NOT "
	}
	return fmt.Sprintf("(%s %sLIKE %s)", l.Expr.String(), not, l.Pattern.String())
}

// SelectItem is one item in the SELECT list.
// Star and Expr are mutually exclusive.
type SelectItem struct {
	Expr  Expression
	Alias string // empty if no AS alias
	Star  bool   // true for * or table.*
	Table string // non-empty for table.*, empty for bare *
}

func (s *SelectItem) String() string {
	var b strings.Builder
	if s.Star {
		if s.Table != "" {
			b.WriteString(s.Table)
			b.WriteByte('.')
		}
		b.WriteByte('*')
	} else {
		b.WriteString(s.Expr.String())
		if s.Alias != "" {
			b.WriteString(" AS ")
			b.WriteString(s.Alias)
		}
	}
	return b.String()
}

// JoinKind is INNER, LEFT, RIGHT, or CROSS.
type JoinKind string

const (
	InnerJoin JoinKind = "INNER"
	LeftJoin  JoinKind = "LEFT"
	RightJoin JoinKind = "RIGHT"
	CrossJoin JoinKind = "CROSS"
)

// JoinClause is one JOIN clause in a FROM list.
// On is nil for CROSS JOIN (no ON condition).
type JoinClause struct {
	Kind      JoinKind
	TableName string
	Alias     string
	On        Expression
}

func (j *JoinClause) String() string {
	var b strings.Builder
	b.WriteString(string(j.Kind))
	b.WriteString(" JOIN ")
	b.WriteString(j.TableName)
	if j.Alias != "" {
		b.WriteString(" AS ")
		b.WriteString(j.Alias)
	}
	if j.On != nil {
		b.WriteString(" ON ")
		b.WriteString(j.On.String())
	}
	return b.String()
}

// OrderItem is one term in an ORDER BY clause.
type OrderItem struct {
	Expr Expression
	Desc bool
}

func (o *OrderItem) String() string {
	dir := " ASC"
	if o.Desc {
		dir = " DESC"
	}
	return o.Expr.String() + dir
}

// ColumnType is one of the four supported SQL column types.
type ColumnType string

const (
	ColTypeInteger ColumnType = "INTEGER"
	ColTypeText    ColumnType = "TEXT"
	ColTypeReal    ColumnType = "REAL"
	ColTypeBoolean ColumnType = "BOOLEAN"
)

// ColumnDef is one column definition in a CREATE TABLE statement.
type ColumnDef struct {
	Name       string
	Type       ColumnType
	NotNull    bool
	PrimaryKey bool
	Default    Expression // nil if no DEFAULT clause
}

func (c *ColumnDef) String() string {
	var b strings.Builder
	b.WriteString(c.Name)
	b.WriteByte(' ')
	b.WriteString(string(c.Type))
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if c.PrimaryKey {
		b.WriteString(" PRIMARY KEY")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(c.Default.String())
	}
	return b.String()
}

// Assignment is one col = expr pair in an UPDATE SET clause.
type Assignment struct {
	Column string
	Value  Expression
}

// --- Concrete statement types ---

// SelectStatement covers the full SELECT syntax with optional clauses.
type SelectStatement struct {
	Distinct  bool
	Columns   []*SelectItem
	From      string
	FromAlias string
	Joins     []*JoinClause
	Where     Expression
	GroupBy   []Expression
	Having    Expression
	OrderBy   []*OrderItem
	Limit     Expression
	Offset    Expression
}

func (*SelectStatement) stmtNode() {}

func (s *SelectStatement) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	cols := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = c.String()
	}
	b.WriteString(strings.Join(cols, ", "))
	if s.From != "" {
		b.WriteString(" FROM ")
		b.WriteString(s.From)
		if s.FromAlias != "" {
			b.WriteString(" AS ")
			b.WriteString(s.FromAlias)
		}
	}
	for _, j := range s.Joins {
		b.WriteByte(' ')
		b.WriteString(j.String())
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	if len(s.GroupBy) > 0 {
		gbs := make([]string, len(s.GroupBy))
		for i, e := range s.GroupBy {
			gbs[i] = e.String()
		}
		b.WriteString(" GROUP BY ")
		b.WriteString(strings.Join(gbs, ", "))
	}
	if s.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(s.Having.String())
	}
	if len(s.OrderBy) > 0 {
		obs := make([]string, len(s.OrderBy))
		for i, o := range s.OrderBy {
			obs[i] = o.String()
		}
		b.WriteString(" ORDER BY ")
		b.WriteString(strings.Join(obs, ", "))
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(s.Limit.String())
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET ")
		b.WriteString(s.Offset.String())
	}
	return b.String()
}

// InsertStatement covers INSERT INTO … VALUES …
type InsertStatement struct {
	Table   string
	Columns []string       // empty if no column list
	Rows    [][]Expression // one slice per value row
}

func (*InsertStatement) stmtNode() {}

func (s *InsertStatement) String() string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(s.Table)
	if len(s.Columns) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(s.Columns, ", "))
		b.WriteByte(')')
	}
	b.WriteString(" VALUES ")
	rows := make([]string, len(s.Rows))
	for i, row := range s.Rows {
		vals := make([]string, len(row))
		for j, v := range row {
			vals[j] = v.String()
		}
		rows[i] = "(" + strings.Join(vals, ", ") + ")"
	}
	b.WriteString(strings.Join(rows, ", "))
	return b.String()
}

// UpdateStatement covers UPDATE … SET … [WHERE …]
type UpdateStatement struct {
	Table       string
	Assignments []Assignment
	Where       Expression
}

func (*UpdateStatement) stmtNode() {}

func (s *UpdateStatement) String() string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(s.Table)
	b.WriteString(" SET ")
	asgns := make([]string, len(s.Assignments))
	for i, a := range s.Assignments {
		asgns[i] = a.Column + " = " + a.Value.String()
	}
	b.WriteString(strings.Join(asgns, ", "))
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	return b.String()
}

// DeleteStatement covers DELETE FROM … [WHERE …]
type DeleteStatement struct {
	Table string
	Where Expression
}

func (*DeleteStatement) stmtNode() {}

func (s *DeleteStatement) String() string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(s.Table)
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	return b.String()
}

// CreateTableStatement covers CREATE TABLE [IF NOT EXISTS] name (…)
type CreateTableStatement struct {
	Name        string
	IfNotExists bool
	Columns     []*ColumnDef
	PrimaryKey  []string // table-level PRIMARY KEY columns, if any
}

func (*CreateTableStatement) stmtNode() {}

func (s *CreateTableStatement) String() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	if s.IfNotExists {
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteString(s.Name)
	b.WriteString(" (")
	cols := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = c.String()
	}
	b.WriteString(strings.Join(cols, ", "))
	if len(s.PrimaryKey) > 0 {
		b.WriteString(", PRIMARY KEY (")
		b.WriteString(strings.Join(s.PrimaryKey, ", "))
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}

// DropTableStatement covers DROP TABLE [IF EXISTS] name
type DropTableStatement struct {
	Name     string
	IfExists bool
}

func (*DropTableStatement) stmtNode() {}

func (s *DropTableStatement) String() string {
	if s.IfExists {
		return "DROP TABLE IF EXISTS " + s.Name
	}
	return "DROP TABLE " + s.Name
}

// BeginStatement, CommitStatement, RollbackStatement are transaction controls.
type BeginStatement struct{}
type CommitStatement struct{}
type RollbackStatement struct{}

func (*BeginStatement) stmtNode()    {}
func (*CommitStatement) stmtNode()   {}
func (*RollbackStatement) stmtNode() {}

func (*BeginStatement) String() string    { return "BEGIN" }
func (*CommitStatement) String() string   { return "COMMIT" }
func (*RollbackStatement) String() string { return "ROLLBACK" }
```

Every node implements `String()`, and every `BinaryExpr` prints with surrounding parentheses. That is what makes round-tripping reliable: the printed SQL is fully parenthesized, so re-parsing it cannot pick a different precedence. The marker methods (`stmtNode`, `exprNode`) keep an unrelated type from accidentally satisfying `Statement` or `Expression`.

Create `parser.go`:

```go
package parser

import (
	"errors"
	"fmt"
	"strings"

	"example.com/sqlparser/lexer"
)

// ErrSyntax is the root cause wrapped into every ParseError.
// Test with: errors.Is(err, ErrSyntax).
var ErrSyntax = errors.New("syntax error")

// ErrUnexpectedEOF is returned when the input ends before a statement completes.
var ErrUnexpectedEOF = errors.New("unexpected end of input")

// ParseError carries the source location and a description of the problem.
// It wraps ErrSyntax via Unwrap, enabling errors.Is(err, ErrSyntax).
type ParseError struct {
	Line int
	Col  int
	Msg  string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Line, e.Col, e.Msg)
}

func (e *ParseError) Unwrap() error { return ErrSyntax }

// Parser converts the token stream from a lexer.Lexer into an AST.
// It keeps one token of lookahead: cur is the token under examination,
// peek is the next token after cur.
type Parser struct {
	lex  *lexer.Lexer
	cur  lexer.Token
	peek lexer.Token
	errs []*ParseError
}

// New creates a Parser for l and primes the two-token lookahead buffer.
func New(l *lexer.Lexer) *Parser {
	p := &Parser{lex: l}
	p.nextToken()
	p.nextToken()
	return p
}

// Errors returns all parse errors accumulated so far.
func (p *Parser) Errors() []*ParseError { return p.errs }

func (p *Parser) nextToken() {
	p.cur = p.peek
	p.peek = p.lex.NextToken()
}

func (p *Parser) curIs(tt lexer.TokenType) bool  { return p.cur.Type == tt }
func (p *Parser) peekIs(tt lexer.TokenType) bool { return p.peek.Type == tt }

// expect asserts cur.Type == tt, then advances. Returns false and records an
// error on mismatch; returns true and advances on match.
func (p *Parser) expect(tt lexer.TokenType) bool {
	if p.cur.Type == tt {
		p.nextToken()
		return true
	}
	p.addErr(p.cur.Line, p.cur.Col,
		fmt.Sprintf("expected %v, got %q", tt, p.cur.Literal))
	return false
}

func (p *Parser) addErr(line, col int, msg string) {
	p.errs = append(p.errs, &ParseError{Line: line, Col: col, Msg: msg})
}

func (p *Parser) syntaxErr(format string, args ...any) {
	p.addErr(p.cur.Line, p.cur.Col, fmt.Sprintf(format, args...))
}

func (p *Parser) firstErr() error {
	if len(p.errs) == 0 {
		return nil
	}
	return p.errs[0]
}

// ParseStatement parses one SQL statement, consuming the optional trailing
// semicolon. It returns (nil, ErrUnexpectedEOF) at end of input.
func (p *Parser) ParseStatement() (Statement, error) {
	for p.curIs(lexer.TokenSemicolon) {
		p.nextToken()
	}
	if p.curIs(lexer.TokenEOF) {
		return nil, fmt.Errorf("%w", ErrUnexpectedEOF)
	}

	var stmt Statement
	switch p.cur.Type {
	case lexer.TokenSelect:
		stmt = p.parseSelect()
	case lexer.TokenInsert:
		stmt = p.parseInsert()
	case lexer.TokenUpdate:
		stmt = p.parseUpdate()
	case lexer.TokenDelete:
		stmt = p.parseDelete()
	case lexer.TokenCreate:
		stmt = p.parseCreate()
	case lexer.TokenDrop:
		stmt = p.parseDrop()
	case lexer.TokenBegin:
		p.nextToken()
		stmt = &BeginStatement{}
	case lexer.TokenCommit:
		p.nextToken()
		stmt = &CommitStatement{}
	case lexer.TokenRollback:
		p.nextToken()
		stmt = &RollbackStatement{}
	default:
		p.syntaxErr("unexpected token %q at start of statement", p.cur.Literal)
		return nil, p.firstErr()
	}

	if p.curIs(lexer.TokenSemicolon) {
		p.nextToken()
	}
	return stmt, p.firstErr()
}

// ParseAll parses every statement in the input and returns them as a slice.
// It stops on the first error.
func (p *Parser) ParseAll() ([]Statement, error) {
	var stmts []Statement
	for !p.curIs(lexer.TokenEOF) {
		stmt, err := p.ParseStatement()
		if err != nil {
			return stmts, err
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}
	return stmts, nil
}

// --- SELECT ---

func (p *Parser) parseSelect() *SelectStatement {
	p.nextToken() // consume SELECT
	sel := &SelectStatement{}

	if p.curIs(lexer.TokenDistinct) {
		sel.Distinct = true
		p.nextToken()
	}

	sel.Columns = p.parseSelectList()

	if p.curIs(lexer.TokenFrom) {
		p.nextToken()
		sel.From = p.cur.Literal
		p.nextToken()
		if p.curIs(lexer.TokenAs) {
			p.nextToken()
			sel.FromAlias = p.cur.Literal
			p.nextToken()
		}
		sel.Joins = p.parseJoins()
	}

	if p.curIs(lexer.TokenWhere) {
		if sel.From == "" {
			p.syntaxErr("WHERE requires a preceding FROM clause, got %q", p.cur.Literal)
		}
		p.nextToken()
		sel.Where = p.parseExpression(0)
	}

	if p.curIs(lexer.TokenGroup) && p.peekIs(lexer.TokenBy) {
		p.nextToken()
		p.nextToken()
		sel.GroupBy = p.parseExpressionList()
	}

	if p.curIs(lexer.TokenHaving) {
		p.nextToken()
		sel.Having = p.parseExpression(0)
	}

	if p.curIs(lexer.TokenOrder) && p.peekIs(lexer.TokenBy) {
		p.nextToken()
		p.nextToken()
		sel.OrderBy = p.parseOrderList()
	}

	if p.curIs(lexer.TokenLimit) {
		p.nextToken()
		sel.Limit = p.parseExpression(0)
	}

	if p.curIs(lexer.TokenOffset) {
		p.nextToken()
		sel.Offset = p.parseExpression(0)
	}

	return sel
}

func (p *Parser) parseSelectList() []*SelectItem {
	var items []*SelectItem
	for {
		items = append(items, p.parseSelectItem())
		if !p.curIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}
	return items
}

func (p *Parser) parseSelectItem() *SelectItem {
	item := &SelectItem{}

	// Bare *
	if p.curIs(lexer.TokenAsterisk) {
		item.Star = true
		p.nextToken()
		return item
	}

	// table.* needs three tokens of lookahead (identifier, '.', '*'), one more
	// than the parser buffers, so it cannot be left to the expression layer:
	// parseIdentOrCall already turns table.column into a ColumnRef, but it has
	// no way to represent the '*' wildcard. Detect table.* here by consuming
	// the identifier and the dot, then testing cur. When cur is not '*', the
	// qualified column becomes the left operand of a full expression via
	// parseInfix, so an infix operator after it is no longer dropped:
	// SELECT t.col + 1 FROM t keeps the "+ 1".
	if (p.curIs(lexer.TokenIdent) || p.curIs(lexer.TokenQIdent)) &&
		p.peekIs(lexer.TokenDot) {
		name := p.cur.Literal
		p.nextToken() // consume identifier
		p.nextToken() // consume .
		if p.curIs(lexer.TokenAsterisk) {
			item.Star = true
			item.Table = name
			p.nextToken()
			return item
		}
		col := &ColumnRef{Table: name, Column: p.cur.Literal}
		p.nextToken() // consume column
		item.Expr = p.parseInfix(col, 0)
	} else {
		item.Expr = p.parseExpression(0)
	}

	if p.curIs(lexer.TokenAs) {
		p.nextToken()
		item.Alias = p.cur.Literal
		p.nextToken()
	}
	return item
}

func (p *Parser) parseJoins() []*JoinClause {
	var joins []*JoinClause
	for {
		var kind JoinKind
		switch p.cur.Type {
		case lexer.TokenJoin:
			kind = InnerJoin
		case lexer.TokenInner:
			p.nextToken()
			if !p.curIs(lexer.TokenJoin) {
				return joins
			}
			kind = InnerJoin
		case lexer.TokenLeft:
			p.nextToken()
			if p.curIs(lexer.TokenOuter) {
				p.nextToken()
			}
			if !p.curIs(lexer.TokenJoin) {
				return joins
			}
			kind = LeftJoin
		case lexer.TokenRight:
			p.nextToken()
			if p.curIs(lexer.TokenOuter) {
				p.nextToken()
			}
			if !p.curIs(lexer.TokenJoin) {
				return joins
			}
			kind = RightJoin
		default:
			return joins
		}

		p.nextToken() // consume JOIN
		j := &JoinClause{Kind: kind, TableName: p.cur.Literal}
		p.nextToken()
		if p.curIs(lexer.TokenAs) {
			p.nextToken()
			j.Alias = p.cur.Literal
			p.nextToken()
		}
		if p.curIs(lexer.TokenOn) {
			p.nextToken()
			j.On = p.parseExpression(0)
		}
		joins = append(joins, j)
	}
}

func (p *Parser) parseExpressionList() []Expression {
	var exprs []Expression
	for {
		exprs = append(exprs, p.parseExpression(0))
		if !p.curIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}
	return exprs
}

func (p *Parser) parseOrderList() []*OrderItem {
	var items []*OrderItem
	for {
		e := p.parseExpression(0)
		desc := false
		if p.curIs(lexer.TokenDesc) {
			desc = true
			p.nextToken()
		} else if p.curIs(lexer.TokenAsc) {
			p.nextToken()
		}
		items = append(items, &OrderItem{Expr: e, Desc: desc})
		if !p.curIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}
	return items
}

// --- INSERT ---

func (p *Parser) parseInsert() *InsertStatement {
	p.nextToken() // consume INSERT
	if !p.curIs(lexer.TokenInto) {
		p.syntaxErr("expected INTO after INSERT, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume INTO
	stmt := &InsertStatement{Table: p.cur.Literal}
	p.nextToken()

	if p.curIs(lexer.TokenLParen) {
		p.nextToken()
		for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
			stmt.Columns = append(stmt.Columns, p.cur.Literal)
			p.nextToken()
			if p.curIs(lexer.TokenComma) {
				p.nextToken()
			}
		}
		if !p.expect(lexer.TokenRParen) {
			return nil
		}
	}

	if !p.curIs(lexer.TokenValues) {
		p.syntaxErr("expected VALUES, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken()

	for {
		if !p.curIs(lexer.TokenLParen) {
			p.syntaxErr("expected ( to start value row, got %q", p.cur.Literal)
			return nil
		}
		p.nextToken()
		var row []Expression
		for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
			row = append(row, p.parseExpression(0))
			if p.curIs(lexer.TokenComma) {
				p.nextToken()
			}
		}
		if !p.expect(lexer.TokenRParen) {
			return nil
		}
		stmt.Rows = append(stmt.Rows, row)
		if !p.curIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}
	return stmt
}

// --- UPDATE ---

func (p *Parser) parseUpdate() *UpdateStatement {
	p.nextToken() // consume UPDATE
	stmt := &UpdateStatement{Table: p.cur.Literal}
	p.nextToken()

	if !p.curIs(lexer.TokenSet) {
		p.syntaxErr("expected SET, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken()

	for {
		col := p.cur.Literal
		p.nextToken()
		if !p.curIs(lexer.TokenEq) {
			p.syntaxErr("expected = in SET clause, got %q", p.cur.Literal)
			return nil
		}
		p.nextToken()
		val := p.parseExpression(0)
		stmt.Assignments = append(stmt.Assignments, Assignment{Column: col, Value: val})
		if !p.curIs(lexer.TokenComma) {
			break
		}
		p.nextToken()
	}

	if p.curIs(lexer.TokenWhere) {
		p.nextToken()
		stmt.Where = p.parseExpression(0)
	}
	return stmt
}

// --- DELETE ---

func (p *Parser) parseDelete() *DeleteStatement {
	p.nextToken() // consume DELETE
	if !p.curIs(lexer.TokenFrom) {
		p.syntaxErr("expected FROM after DELETE, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken()
	stmt := &DeleteStatement{Table: p.cur.Literal}
	p.nextToken()
	if p.curIs(lexer.TokenWhere) {
		p.nextToken()
		stmt.Where = p.parseExpression(0)
	}
	return stmt
}

// --- CREATE ---

func (p *Parser) parseCreate() Statement {
	p.nextToken() // consume CREATE
	if !p.curIs(lexer.TokenTable) {
		p.syntaxErr("expected TABLE after CREATE, got %q", p.cur.Literal)
		return nil
	}
	return p.parseCreateTable()
}

func (p *Parser) parseCreateTable() *CreateTableStatement {
	p.nextToken() // consume TABLE
	stmt := &CreateTableStatement{}

	// IF NOT EXISTS: IF is not a keyword, NOT and EXISTS are.
	if p.curIs(lexer.TokenIdent) &&
		strings.ToUpper(p.cur.Literal) == "IF" &&
		p.peekIs(lexer.TokenNot) {
		p.nextToken() // consume IF
		p.nextToken() // consume NOT
		if p.curIs(lexer.TokenExists) {
			stmt.IfNotExists = true
			p.nextToken()
		}
	}

	stmt.Name = p.cur.Literal
	p.nextToken()

	if !p.curIs(lexer.TokenLParen) {
		p.syntaxErr("expected ( after table name, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken()

	for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
		// Table-level PRIMARY KEY (cols…)
		if p.curIs(lexer.TokenPrimary) && p.peekIs(lexer.TokenKey) {
			p.nextToken()
			p.nextToken()
			if !p.expect(lexer.TokenLParen) {
				return nil
			}
			for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
				stmt.PrimaryKey = append(stmt.PrimaryKey, p.cur.Literal)
				p.nextToken()
				if p.curIs(lexer.TokenComma) {
					p.nextToken()
				}
			}
			if !p.expect(lexer.TokenRParen) {
				return nil
			}
		} else {
			col := p.parseColumnDef()
			if col != nil {
				stmt.Columns = append(stmt.Columns, col)
			}
		}
		if p.curIs(lexer.TokenComma) {
			p.nextToken()
		}
	}
	if !p.expect(lexer.TokenRParen) {
		return nil
	}
	return stmt
}

func (p *Parser) parseColumnDef() *ColumnDef {
	col := &ColumnDef{Name: p.cur.Literal}
	p.nextToken()

	switch p.cur.Type {
	case lexer.TokenInteger:
		col.Type = ColTypeInteger
	case lexer.TokenText:
		col.Type = ColTypeText
	case lexer.TokenReal:
		col.Type = ColTypeReal
	case lexer.TokenBoolean:
		col.Type = ColTypeBoolean
	default:
		p.syntaxErr("expected column type (INTEGER, TEXT, REAL, BOOLEAN), got %q", p.cur.Literal)
		return nil
	}
	p.nextToken()

	for {
		switch {
		case p.curIs(lexer.TokenNot) && p.peekIs(lexer.TokenNull):
			col.NotNull = true
			p.nextToken()
			p.nextToken()
		case p.curIs(lexer.TokenPrimary) && p.peekIs(lexer.TokenKey):
			col.PrimaryKey = true
			p.nextToken()
			p.nextToken()
		case p.curIs(lexer.TokenIdent) &&
			strings.ToUpper(p.cur.Literal) == "DEFAULT":
			p.nextToken()
			col.Default = p.parseExpression(0)
		default:
			return col
		}
	}
}

// --- DROP ---

func (p *Parser) parseDrop() Statement {
	p.nextToken() // consume DROP
	if !p.curIs(lexer.TokenTable) {
		p.syntaxErr("expected TABLE after DROP, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume TABLE
	stmt := &DropTableStatement{}

	// IF EXISTS: IF is not a keyword, EXISTS is.
	if p.curIs(lexer.TokenIdent) &&
		strings.ToUpper(p.cur.Literal) == "IF" &&
		p.peekIs(lexer.TokenExists) {
		p.nextToken() // consume IF
		p.nextToken() // consume EXISTS
		stmt.IfExists = true
	}

	stmt.Name = p.cur.Literal
	p.nextToken()
	return stmt
}

// --- Expression parser (Pratt) ---

// infixBP returns the left (lbp) and right (rbp) binding powers for an infix
// operator. ok is false when tt is not an infix operator.
//
// Precedence table (higher = tighter binding):
//
//	OR          lbp=1  rbp=2
//	AND         lbp=3  rbp=4
//	Comparison  lbp=5  rbp=6   (=, !=, <, >, <=, >=)
//	Addition    lbp=7  rbp=8   (+, -)
//	Multiply    lbp=9  rbp=10  (*, /)
func infixBP(tt lexer.TokenType) (lbp, rbp int, ok bool) {
	switch tt {
	case lexer.TokenOr:
		return 1, 2, true
	case lexer.TokenAnd:
		return 3, 4, true
	case lexer.TokenEq, lexer.TokenNeq,
		lexer.TokenLt, lexer.TokenGt,
		lexer.TokenLtEq, lexer.TokenGtEq:
		return 5, 6, true
	case lexer.TokenPlus, lexer.TokenMinus:
		return 7, 8, true
	case lexer.TokenAsterisk, lexer.TokenSlash:
		return 9, 10, true
	}
	return 0, 0, false
}

// cmpBP is the binding power at which IS, BETWEEN, IN, and LIKE are absorbed.
// Special forms are only absorbed when their lbp (5) exceeds minBP.
const cmpBP = 5

// parseExpression is the Pratt entry point. minBP is the minimum left binding
// power the next infix operator must exceed for it to be absorbed.
func (p *Parser) parseExpression(minBP int) Expression {
	left := p.parsePrefix()
	if left == nil {
		return nil
	}
	return p.parseInfix(left, minBP)
}

// parseInfix runs the Pratt left-denotation (led) loop over an already-parsed
// left operand, absorbing infix operators and special SQL forms whose left
// binding power exceeds minBP. Splitting it out of parseExpression lets
// parseSelectItem continue an expression after a qualified column that it had
// to inspect for a trailing star, and lets other predicate entry points reuse
// the operator loop instead of duplicating it.
func (p *Parser) parseInfix(left Expression, minBP int) Expression {
	for {
		// Special SQL infix forms at comparison binding power.
		// Only absorb them when cmpBP > minBP (same rule as for regular infix).
		if cmpBP > minBP {
			switch p.cur.Type {
			case lexer.TokenIs:
				p.nextToken() // consume IS
				notNull := false
				if p.curIs(lexer.TokenNot) {
					notNull = true
					p.nextToken()
				}
				if !p.curIs(lexer.TokenNull) {
					p.syntaxErr("expected NULL after IS [NOT], got %q", p.cur.Literal)
					return left
				}
				p.nextToken()
				left = &IsNullExpr{Expr: left, IsNot: notNull}
				continue
			case lexer.TokenBetween:
				left = p.parseBetween(left, false)
				continue
			case lexer.TokenIn:
				left = p.parseIn(left, false)
				continue
			case lexer.TokenLike:
				left = p.parseLike(left, false)
				continue
			case lexer.TokenNot:
				if p.peekIs(lexer.TokenBetween) {
					p.nextToken() // cur = BETWEEN
					left = p.parseBetween(left, true)
					continue
				}
				if p.peekIs(lexer.TokenIn) {
					p.nextToken() // cur = IN
					left = p.parseIn(left, true)
					continue
				}
				if p.peekIs(lexer.TokenLike) {
					p.nextToken() // cur = LIKE
					left = p.parseLike(left, true)
					continue
				}
			}
		}

		lbp, rbp, ok := infixBP(p.cur.Type)
		if !ok || lbp <= minBP {
			break
		}
		op := p.cur.Literal
		p.nextToken()
		right := p.parseExpression(rbp)
		left = &BinaryExpr{Left: left, Op: op, Right: right}
	}

	return left
}

// parseBetween handles: expr [NOT] BETWEEN lo AND hi.
// lo and hi are parsed with minBP=cmpBP to stop at AND and at comparison
// operators, preventing them from being absorbed into the bounds.
func (p *Parser) parseBetween(expr Expression, not bool) Expression {
	p.nextToken() // consume BETWEEN
	lo := p.parseExpression(cmpBP)
	if !p.curIs(lexer.TokenAnd) {
		p.syntaxErr("expected AND in BETWEEN expression, got %q", p.cur.Literal)
		return expr
	}
	p.nextToken() // consume AND
	hi := p.parseExpression(cmpBP)
	return &BetweenExpr{Expr: expr, Not: not, Lo: lo, Hi: hi}
}

// parseIn handles: expr [NOT] IN ( values… ) or expr [NOT] IN ( subquery ).
func (p *Parser) parseIn(expr Expression, not bool) Expression {
	p.nextToken() // consume IN
	if !p.curIs(lexer.TokenLParen) {
		p.syntaxErr("expected ( after IN, got %q", p.cur.Literal)
		return expr
	}
	p.nextToken()
	in := &InExpr{Expr: expr, Not: not}

	if p.curIs(lexer.TokenSelect) {
		in.Subquery = p.parseSelect()
		if !p.expect(lexer.TokenRParen) {
			return in
		}
	} else {
		for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
			in.Values = append(in.Values, p.parseExpression(0))
			if p.curIs(lexer.TokenComma) {
				p.nextToken()
			}
		}
		if !p.expect(lexer.TokenRParen) {
			return in
		}
	}
	return in
}

// parseLike handles: expr [NOT] LIKE pattern.
// The pattern is parsed with minBP=cmpBP to stop before comparison operators.
func (p *Parser) parseLike(expr Expression, not bool) Expression {
	p.nextToken() // consume LIKE
	pattern := p.parseExpression(cmpBP)
	return &LikeExpr{Expr: expr, Not: not, Pattern: pattern}
}

// parsePrefix handles the null-denotation: atoms, unary operators, parentheses.
func (p *Parser) parsePrefix() Expression {
	switch p.cur.Type {
	case lexer.TokenInt:
		lit := &LiteralExpr{Kind: LiteralInt, Value: p.cur.Literal}
		p.nextToken()
		return lit

	case lexer.TokenFloat:
		lit := &LiteralExpr{Kind: LiteralFloat, Value: p.cur.Literal}
		p.nextToken()
		return lit

	case lexer.TokenString:
		lit := &LiteralExpr{Kind: LiteralString, Value: p.cur.Literal}
		p.nextToken()
		return lit

	case lexer.TokenTrue, lexer.TokenFalse:
		lit := &LiteralExpr{Kind: LiteralBool, Value: p.cur.Literal}
		p.nextToken()
		return lit

	case lexer.TokenNull:
		p.nextToken()
		return &LiteralExpr{Kind: LiteralNull}

	case lexer.TokenMinus:
		p.nextToken()
		operand := p.parseExpression(11) // tighter than multiplication
		return &UnaryExpr{Op: "-", Operand: operand}

	case lexer.TokenNot:
		p.nextToken()
		operand := p.parseExpression(2) // just above OR
		return &UnaryExpr{Op: "NOT", Operand: operand}

	case lexer.TokenLParen:
		p.nextToken()
		if p.curIs(lexer.TokenSelect) {
			sub := p.parseSelect()
			if !p.expect(lexer.TokenRParen) {
				return nil
			}
			return &SubqueryExpr{Query: sub}
		}
		inner := p.parseExpression(0)
		if !p.expect(lexer.TokenRParen) {
			return inner
		}
		return inner

	case lexer.TokenIdent, lexer.TokenQIdent,
		lexer.TokenCount, lexer.TokenSum, lexer.TokenAvg,
		lexer.TokenMin, lexer.TokenMax:
		return p.parseIdentOrCall()

	default:
		p.syntaxErr("unexpected token %q in expression", p.cur.Literal)
		p.nextToken()
		return nil
	}
}

// parseIdentOrCall handles bare identifiers, table.column, and function calls.
func (p *Parser) parseIdentOrCall() Expression {
	name := p.cur.Literal
	p.nextToken()

	// Function call: name(args…)
	if p.curIs(lexer.TokenLParen) {
		return p.parseFunctionCall(name)
	}

	// Qualified reference: table.column
	if p.curIs(lexer.TokenDot) {
		p.nextToken()
		col := p.cur.Literal
		p.nextToken()
		return &ColumnRef{Table: name, Column: col}
	}

	return &ColumnRef{Column: name}
}

// parseFunctionCall handles name(…), name(*), name(DISTINCT …).
func (p *Parser) parseFunctionCall(name string) Expression {
	p.nextToken() // consume (
	fn := &FunctionCallExpr{Name: strings.ToUpper(name)}

	if p.curIs(lexer.TokenAsterisk) {
		fn.Star = true
		p.nextToken()
		p.expect(lexer.TokenRParen)
		return fn
	}

	if p.curIs(lexer.TokenDistinct) {
		fn.Distinct = true
		p.nextToken()
	}

	for !p.curIs(lexer.TokenRParen) && !p.curIs(lexer.TokenEOF) {
		fn.Args = append(fn.Args, p.parseExpression(0))
		if p.curIs(lexer.TokenComma) {
			p.nextToken()
		}
	}
	p.expect(lexer.TokenRParen)
	return fn
}
```

Read the led loop in `parseInfix` carefully: the special-form block is the entire reason a single binding power (`cmpBP`) governs `IS NULL`, `BETWEEN`, `IN`, and `LIKE`. The block is entered only when `cmpBP > minBP`, so on the right side of `+` (where `minBP` is 8) the predicates are skipped and a trailing `NOT IN` attaches at the outer comparison level instead of inside the arithmetic. The `parseBetween` helper parses both bounds at `cmpBP`, which is what leaves the `AND` token for the helper to consume rather than letting the operator loop eat it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	parser "example.com/sqlparser"
	"example.com/sqlparser/lexer"
)

func main() {
	input := `
		CREATE TABLE users (
			id     INTEGER PRIMARY KEY,
			name   TEXT NOT NULL,
			email  TEXT,
			active BOOLEAN DEFAULT TRUE
		);
		INSERT INTO users (id, name, email) VALUES (1, 'Alice', 'alice@example.com'), (2, 'Bob', 'bob@example.com');
		SELECT u.id, u.name FROM users AS u
			LEFT JOIN orders AS o ON u.id = o.user_id
			WHERE u.active = TRUE
			ORDER BY u.name ASC
			LIMIT 20;
		UPDATE users SET active = FALSE WHERE id = 2;
		DELETE FROM users WHERE active = FALSE AND id > 100;
		BEGIN;
		COMMIT;
		DROP TABLE IF EXISTS tmp_import;
	`

	l := lexer.New(input)
	p := parser.New(l)
	stmts, err := p.ParseAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
		if errs := p.Errors(); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  %v\n", e)
			}
		}
		os.Exit(1)
	}

	for i, stmt := range stmts {
		fmt.Printf("[%d] %s\n\n", i+1, stmt.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (note the explicit parentheses that the `BinaryExpr` printer adds, and that `DELETE`'s `AND` groups its two comparisons):

```
[1] CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT, active BOOLEAN DEFAULT TRUE)

[2] INSERT INTO users (id, name, email) VALUES (1, 'Alice', 'alice@example.com'), (2, 'Bob', 'bob@example.com')

[3] SELECT u.id, u.name FROM users AS u LEFT JOIN orders AS o ON (u.id = o.user_id) WHERE (u.active = TRUE) ORDER BY u.name ASC LIMIT 20

[4] UPDATE users SET active = FALSE WHERE (id = 2)

[5] DELETE FROM users WHERE ((active = FALSE) AND (id > 100))

[6] BEGIN

[7] COMMIT

[8] DROP TABLE IF EXISTS tmp_import
```

Create `parser_test.go`:

```go
package parser

import (
	"errors"
	"fmt"
	"testing"

	"example.com/sqlparser/lexer"
)

// parse is a test helper: parses one statement from sql and returns it.
func parse(t *testing.T, sql string) Statement {
	t.Helper()
	l := lexer.New(sql)
	p := New(l)
	stmt, err := p.ParseStatement()
	if err != nil {
		t.Fatalf("ParseStatement(%q) error: %v", sql, err)
	}
	return stmt
}

// parseErr is a test helper: asserts that sql produces a parse error.
func parseErr(t *testing.T, sql string) error {
	t.Helper()
	l := lexer.New(sql)
	p := New(l)
	_, err := p.ParseStatement()
	if err == nil {
		t.Fatalf("ParseStatement(%q): expected error, got nil", sql)
	}
	return err
}

func ExampleNew() {
	l := lexer.New("SELECT id FROM users WHERE id = 1")
	p := New(l)
	stmt, err := p.ParseStatement()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(stmt.String())
	// Output: SELECT id FROM users WHERE (id = 1)
}

func TestParseSelectBasic(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id, name FROM users")
	sel, ok := stmt.(*SelectStatement)
	if !ok {
		t.Fatalf("got %T, want *SelectStatement", stmt)
	}
	if sel.Distinct {
		t.Fatal("Distinct should be false")
	}
	if len(sel.Columns) != 2 {
		t.Fatalf("len(Columns) = %d, want 2", len(sel.Columns))
	}
	if sel.From != "users" {
		t.Fatalf("From = %q, want %q", sel.From, "users")
	}
	if sel.Where != nil {
		t.Fatal("Where should be nil")
	}
}

func TestParseSelectDistinct(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT DISTINCT email FROM users")
	sel := stmt.(*SelectStatement)
	if !sel.Distinct {
		t.Fatal("Distinct should be true")
	}
}

func TestParseSelectStar(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT * FROM orders")
	sel := stmt.(*SelectStatement)
	if len(sel.Columns) != 1 || !sel.Columns[0].Star {
		t.Fatalf("Columns = %+v, want [{Star: true}]", sel.Columns)
	}
}

func TestParseSelectTableStar(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT u.* FROM users AS u")
	sel := stmt.(*SelectStatement)
	if len(sel.Columns) != 1 {
		t.Fatalf("len(Columns) = %d, want 1", len(sel.Columns))
	}
	c := sel.Columns[0]
	if !c.Star || c.Table != "u" {
		t.Fatalf("column = %+v, want {Star: true, Table: u}", c)
	}
	if sel.FromAlias != "u" {
		t.Fatalf("FromAlias = %q, want %q", sel.FromAlias, "u")
	}
}

func TestParseSelectQualifiedColumnArithmetic(t *testing.T) {
	t.Parallel()

	// A qualified column must be usable as the left operand of a full
	// expression: t.col + 1 must keep the "+ 1" rather than dropping it.
	stmt := parse(t, "SELECT t.col + 1 FROM t")
	sel := stmt.(*SelectStatement)
	if len(sel.Columns) != 1 {
		t.Fatalf("len(Columns) = %d, want 1", len(sel.Columns))
	}
	bin, ok := sel.Columns[0].Expr.(*BinaryExpr)
	if !ok {
		t.Fatalf("Expr = %T, want *BinaryExpr", sel.Columns[0].Expr)
	}
	if bin.Op != "+" {
		t.Fatalf("Op = %q, want +", bin.Op)
	}
	col, ok := bin.Left.(*ColumnRef)
	if !ok {
		t.Fatalf("Left = %T, want *ColumnRef", bin.Left)
	}
	if col.Table != "t" || col.Column != "col" {
		t.Fatalf("Left = %+v, want {Table: t, Column: col}", col)
	}
	if got := bin.Right.String(); got != "1" {
		t.Fatalf("Right = %q, want 1", got)
	}
	// Round-trip: the regenerated SQL re-parses to the same string.
	if got := sel.Columns[0].Expr.String(); got != "(t.col + 1)" {
		t.Fatalf("Expr.String() = %q, want (t.col + 1)", got)
	}
}

func TestParseSelectWhere(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM users WHERE id = 1")
	sel := stmt.(*SelectStatement)
	if sel.Where == nil {
		t.Fatal("Where should not be nil")
	}
	if got := sel.Where.String(); got != "(id = 1)" {
		t.Fatalf("Where.String() = %q, want %q", got, "(id = 1)")
	}
}

func TestParseSelectInnerJoin(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT u.id, o.total FROM users AS u INNER JOIN orders AS o ON u.id = o.user_id")
	sel := stmt.(*SelectStatement)
	if len(sel.Joins) != 1 {
		t.Fatalf("len(Joins) = %d, want 1", len(sel.Joins))
	}
	j := sel.Joins[0]
	if j.Kind != InnerJoin {
		t.Fatalf("Kind = %v, want InnerJoin", j.Kind)
	}
	if j.TableName != "orders" || j.Alias != "o" {
		t.Fatalf("join = %+v", j)
	}
	if j.On == nil {
		t.Fatal("ON condition should not be nil")
	}
}

func TestParseSelectLeftJoin(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT u.id FROM users AS u LEFT JOIN orders AS o ON u.id = o.user_id")
	sel := stmt.(*SelectStatement)
	if len(sel.Joins) != 1 || sel.Joins[0].Kind != LeftJoin {
		t.Fatalf("want one LEFT JOIN, got %+v", sel.Joins)
	}
}

func TestParseSelectGroupByHaving(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT dept, COUNT(*) FROM employees GROUP BY dept HAVING COUNT(*) > 5")
	sel := stmt.(*SelectStatement)
	if len(sel.GroupBy) != 1 {
		t.Fatalf("len(GroupBy) = %d, want 1", len(sel.GroupBy))
	}
	if sel.Having == nil {
		t.Fatal("Having should not be nil")
	}
}

func TestParseSelectOrderByLimitOffset(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM users ORDER BY name DESC, id ASC LIMIT 10 OFFSET 20")
	sel := stmt.(*SelectStatement)
	if len(sel.OrderBy) != 2 {
		t.Fatalf("len(OrderBy) = %d, want 2", len(sel.OrderBy))
	}
	if !sel.OrderBy[0].Desc {
		t.Fatal("first OrderBy should be DESC")
	}
	if sel.OrderBy[1].Desc {
		t.Fatal("second OrderBy should be ASC")
	}
	if sel.Limit == nil || sel.Limit.String() != "10" {
		t.Fatalf("Limit = %v, want 10", sel.Limit)
	}
	if sel.Offset == nil || sel.Offset.String() != "20" {
		t.Fatalf("Offset = %v, want 20", sel.Offset)
	}
}

func TestOperatorPrecedenceMultiplyBeforeAdd(t *testing.T) {
	t.Parallel()

	// a + b * c should parse as a + (b * c), not (a + b) * c.
	stmt := parse(t, "SELECT a + b * c FROM t")
	sel := stmt.(*SelectStatement)
	got := sel.Columns[0].Expr.String()
	// BinaryExpr wraps with parens; multiplication binds tighter.
	want := "(a + (b * c))"
	if got != want {
		t.Fatalf("expression = %q, want %q", got, want)
	}
}

func TestOperatorPrecedenceParenOverride(t *testing.T) {
	t.Parallel()

	// (a + b) * c: parentheses override the default precedence.
	stmt := parse(t, "SELECT (a + b) * c FROM t")
	sel := stmt.(*SelectStatement)
	got := sel.Columns[0].Expr.String()
	want := "((a + b) * c)"
	if got != want {
		t.Fatalf("expression = %q, want %q", got, want)
	}
}

func TestOperatorPrecedenceAndOverOr(t *testing.T) {
	t.Parallel()

	// a OR b AND c should parse as a OR (b AND c).
	stmt := parse(t, "SELECT id FROM t WHERE a OR b AND c")
	sel := stmt.(*SelectStatement)
	got := sel.Where.String()
	want := "(a OR (b AND c))"
	if got != want {
		t.Fatalf("Where = %q, want %q", got, want)
	}
}

func TestParseIsNull(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE col IS NULL")
	sel := stmt.(*SelectStatement)
	isNull, ok := sel.Where.(*IsNullExpr)
	if !ok || isNull.IsNot {
		t.Fatalf("Where = %T %+v, want *IsNullExpr{IsNot: false}", sel.Where, sel.Where)
	}
}

func TestParseIsNotNull(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE col IS NOT NULL")
	sel := stmt.(*SelectStatement)
	isNull, ok := sel.Where.(*IsNullExpr)
	if !ok || !isNull.IsNot {
		t.Fatalf("Where = %T %+v, want *IsNullExpr{IsNot: true}", sel.Where, sel.Where)
	}
}

func TestParseBetween(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE age BETWEEN 18 AND 65")
	sel := stmt.(*SelectStatement)
	bet, ok := sel.Where.(*BetweenExpr)
	if !ok {
		t.Fatalf("Where = %T, want *BetweenExpr", sel.Where)
	}
	if bet.Not {
		t.Fatal("Not should be false")
	}
	if bet.Lo.String() != "18" || bet.Hi.String() != "65" {
		t.Fatalf("Lo=%s Hi=%s", bet.Lo.String(), bet.Hi.String())
	}
}

func TestParseNotBetween(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE age NOT BETWEEN 1 AND 17")
	sel := stmt.(*SelectStatement)
	bet, ok := sel.Where.(*BetweenExpr)
	if !ok || !bet.Not {
		t.Fatalf("Where = %T %+v, want *BetweenExpr{Not: true}", sel.Where, sel.Where)
	}
}

func TestParseBetweenWithArithmetic(t *testing.T) {
	t.Parallel()

	// Arithmetic inside BETWEEN bounds must not consume the AND separator.
	stmt := parse(t, "SELECT id FROM t WHERE score BETWEEN base + 1 AND base + 10")
	sel := stmt.(*SelectStatement)
	bet, ok := sel.Where.(*BetweenExpr)
	if !ok {
		t.Fatalf("Where = %T, want *BetweenExpr", sel.Where)
	}
	if bet.Lo.String() != "(base + 1)" {
		t.Fatalf("Lo = %q, want (base + 1)", bet.Lo.String())
	}
	if bet.Hi.String() != "(base + 10)" {
		t.Fatalf("Hi = %q, want (base + 10)", bet.Hi.String())
	}
}

func TestParseIn(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE status IN (1, 2, 3)")
	sel := stmt.(*SelectStatement)
	in, ok := sel.Where.(*InExpr)
	if !ok {
		t.Fatalf("Where = %T, want *InExpr", sel.Where)
	}
	if in.Not {
		t.Fatal("Not should be false")
	}
	if len(in.Values) != 3 {
		t.Fatalf("len(Values) = %d, want 3", len(in.Values))
	}
}

func TestParseNotIn(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE status NOT IN (1, 2)")
	sel := stmt.(*SelectStatement)
	in, ok := sel.Where.(*InExpr)
	if !ok || !in.Not {
		t.Fatalf("Where = %T %+v, want *InExpr{Not: true}", sel.Where, sel.Where)
	}
}

func TestParseSelectSubquery(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE id IN (SELECT id FROM src WHERE active = TRUE)")
	sel := stmt.(*SelectStatement)
	in, ok := sel.Where.(*InExpr)
	if !ok {
		t.Fatalf("Where = %T, want *InExpr", sel.Where)
	}
	if in.Subquery == nil {
		t.Fatal("Subquery should not be nil for IN (SELECT ...)")
	}
	if in.Subquery.From != "src" {
		t.Fatalf("Subquery.From = %q, want src", in.Subquery.From)
	}
}

func TestParseLike(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE name LIKE '%alice%'")
	sel := stmt.(*SelectStatement)
	like, ok := sel.Where.(*LikeExpr)
	if !ok || like.Not {
		t.Fatalf("Where = %T %+v, want *LikeExpr{Not: false}", sel.Where, sel.Where)
	}
	if like.Pattern.String() != "'%alice%'" {
		t.Fatalf("Pattern = %q, want '%%alice%%'", like.Pattern.String())
	}
}

func TestParseNotLike(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "SELECT id FROM t WHERE name NOT LIKE 'admin%'")
	sel := stmt.(*SelectStatement)
	like, ok := sel.Where.(*LikeExpr)
	if !ok || !like.Not {
		t.Fatalf("Where = %T %+v, want *LikeExpr{Not: true}", sel.Where, sel.Where)
	}
}

func TestParseInsert(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "INSERT INTO users (id, name) VALUES (1, 'Alice')")
	ins, ok := stmt.(*InsertStatement)
	if !ok {
		t.Fatalf("got %T, want *InsertStatement", stmt)
	}
	if ins.Table != "users" {
		t.Fatalf("Table = %q, want users", ins.Table)
	}
	if len(ins.Columns) != 2 || ins.Columns[0] != "id" {
		t.Fatalf("Columns = %v", ins.Columns)
	}
	if len(ins.Rows) != 1 || len(ins.Rows[0]) != 2 {
		t.Fatalf("Rows = %v", ins.Rows)
	}
}

func TestParseInsertMultiRow(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "INSERT INTO t (a) VALUES (1), (2), (3)")
	ins := stmt.(*InsertStatement)
	if len(ins.Rows) != 3 {
		t.Fatalf("len(Rows) = %d, want 3", len(ins.Rows))
	}
}

func TestParseUpdate(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "UPDATE users SET name = 'Bob', age = 30 WHERE id = 1")
	upd, ok := stmt.(*UpdateStatement)
	if !ok {
		t.Fatalf("got %T, want *UpdateStatement", stmt)
	}
	if upd.Table != "users" {
		t.Fatalf("Table = %q, want users", upd.Table)
	}
	if len(upd.Assignments) != 2 {
		t.Fatalf("len(Assignments) = %d, want 2", len(upd.Assignments))
	}
	if upd.Where == nil {
		t.Fatal("Where should not be nil")
	}
}

func TestParseDelete(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "DELETE FROM sessions WHERE expired = TRUE")
	del, ok := stmt.(*DeleteStatement)
	if !ok {
		t.Fatalf("got %T, want *DeleteStatement", stmt)
	}
	if del.Table != "sessions" {
		t.Fatalf("Table = %q, want sessions", del.Table)
	}
	if del.Where == nil {
		t.Fatal("Where should not be nil")
	}
}

func TestParseCreateTable(t *testing.T) {
	t.Parallel()

	sql := `CREATE TABLE users (
		id      INTEGER PRIMARY KEY,
		name    TEXT NOT NULL,
		email   TEXT,
		active  BOOLEAN DEFAULT TRUE
	)`
	stmt := parse(t, sql)
	ct, ok := stmt.(*CreateTableStatement)
	if !ok {
		t.Fatalf("got %T, want *CreateTableStatement", stmt)
	}
	if ct.Name != "users" {
		t.Fatalf("Name = %q, want users", ct.Name)
	}
	if ct.IfNotExists {
		t.Fatal("IfNotExists should be false")
	}
	if len(ct.Columns) != 4 {
		t.Fatalf("len(Columns) = %d, want 4", len(ct.Columns))
	}
	if !ct.Columns[0].PrimaryKey {
		t.Fatal("first column should have PRIMARY KEY")
	}
	if !ct.Columns[1].NotNull {
		t.Fatal("name column should have NOT NULL")
	}
	if ct.Columns[3].Default == nil {
		t.Fatal("active column should have DEFAULT")
	}
}

func TestParseCreateTableIfNotExists(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "CREATE TABLE IF NOT EXISTS cache (k TEXT, v TEXT)")
	ct := stmt.(*CreateTableStatement)
	if !ct.IfNotExists {
		t.Fatal("IfNotExists should be true")
	}
}

func TestParseDropTable(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "DROP TABLE sessions")
	dt, ok := stmt.(*DropTableStatement)
	if !ok {
		t.Fatalf("got %T, want *DropTableStatement", stmt)
	}
	if dt.Name != "sessions" {
		t.Fatalf("Name = %q, want sessions", dt.Name)
	}
	if dt.IfExists {
		t.Fatal("IfExists should be false")
	}
}

func TestParseDropTableIfExists(t *testing.T) {
	t.Parallel()

	stmt := parse(t, "DROP TABLE IF EXISTS tmp")
	dt := stmt.(*DropTableStatement)
	if !dt.IfExists {
		t.Fatal("IfExists should be true")
	}
}

func TestParseTransactionStatements(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sql  string
		want string
	}{
		{"BEGIN", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"ROLLBACK", "ROLLBACK"},
	}
	for _, tc := range cases {
		stmt := parse(t, tc.sql)
		if got := stmt.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestParseAll(t *testing.T) {
	t.Parallel()

	sql := "SELECT 1; SELECT 2; SELECT 3"
	l := lexer.New(sql)
	p := New(l)
	stmts, err := p.ParseAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("len(stmts) = %d, want 3", len(stmts))
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []string{
		"SELECT id, name FROM users WHERE (id = 1)",
		"SELECT DISTINCT email FROM users",
		"SELECT * FROM orders",
		"INSERT INTO t (a, b) VALUES (1, 'hello')",
		"UPDATE t SET a = 1 WHERE (b = 2)",
		"DELETE FROM t WHERE (id = 99)",
		"DROP TABLE IF EXISTS tmp",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
	}
	for _, want := range cases {
		l := lexer.New(want)
		p := New(l)
		stmt, err := p.ParseStatement()
		if err != nil {
			t.Errorf("parse(%q): %v", want, err)
			continue
		}
		got := stmt.String()
		// Re-parse the regenerated SQL and compare String() output.
		l2 := lexer.New(got)
		p2 := New(l2)
		stmt2, err2 := p2.ParseStatement()
		if err2 != nil {
			t.Errorf("re-parse(%q): %v", got, err2)
			continue
		}
		if stmt2.String() != got {
			t.Errorf("round-trip mismatch:\n  orig:   %q\n  first:  %q\n  second: %q",
				want, got, stmt2.String())
		}
	}
}

func TestSyntaxErrorWrapsErrSyntax(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
	}{
		{"unknown statement", "UPSERT INTO t VALUES (1)"},
		{"missing FROM", "SELECT id WHERE id = 1"},
		{"missing INTO", "INSERT t VALUES (1)"},
		{"missing VALUES", "INSERT INTO t (a) SET (1)"},
		{"missing SET", "UPDATE t WHERE id = 1"},
		{"missing FROM in DELETE", "DELETE t WHERE id = 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := parseErr(t, tc.sql)
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("errors.Is(err, ErrSyntax) = false; err = %v", err)
			}
		})
	}
}

func TestUnexpectedEOFError(t *testing.T) {
	t.Parallel()

	l := lexer.New("")
	p := New(l)
	_, err := p.ParseStatement()
	if !errors.Is(err, ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
}

func TestParseErrorLocation(t *testing.T) {
	t.Parallel()

	l := lexer.New("UPSERT INTO t VALUES (1)")
	p := New(l)
	_, err := p.ParseStatement()
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err type = %T, want *ParseError", err)
	}
	if pe.Line == 0 || pe.Col == 0 {
		t.Fatalf("ParseError has zero location: line=%d col=%d", pe.Line, pe.Col)
	}
}
```

## Review

The parser is correct when every statement type round-trips and precedence is faithful. `a + b * c` must print as `(a + (b * c))` and `a OR b AND c` as `(a OR (b AND c))`; parentheses in the source must survive as a different grouping. The four special predicates must each produce their own node type, `BETWEEN base + 1 AND base + 10` must keep both bounds intact with the `AND` consumed as a separator, and `NOT IN` / `NOT BETWEEN` / `NOT LIKE` must set the negation flag rather than parsing `NOT` as a unary prefix. Every malformed input must yield an error satisfying `errors.Is(err, ErrSyntax)` with a non-zero line and column, and empty input must yield `ErrUnexpectedEOF`. The round-trip test is the strongest single check: it parses, prints, re-parses, and compares, so any precedence or printing bug surfaces as a mismatch.

Watch the three classic traps. Parsing the BETWEEN bounds at `minBP=0` lets the operator loop swallow the `AND`; they must be parsed at `cmpBP`. Treating `NOT` as a unary prefix in infix position breaks `NOT IN`; the led loop must peek past `NOT` for `BETWEEN`/`IN`/`LIKE`. And entering the special-form block when `minBP >= 5` mis-groups `a + b NOT IN (...)`; the `cmpBP > minBP` guard is what keeps the predicates at comparison level.

## Resources

- [Simple but Powerful Pratt Parsing](https://matklad.github.io/2020/04/13/simple-but-powerful-pratt-parsing.html) — the canonical Go-friendly exposition; the binding-power pairs map directly to `infixBP`.
- [Top Down Operator Precedence](https://crockford.com/javascript/tdop/tdop.html) — Douglas Crockford's exposition of Vaughan Pratt's 1973 algorithm and the source of the `nud`/`led` terminology.
- [SQLite: the `expr` grammar](https://www.sqlite.org/lang_expr.html) — operator precedence and the `[NOT] IN/LIKE/BETWEEN`, `IS [NOT] NULL` predicate forms reproduced here.
- [go/ast package](https://pkg.go.dev/go/ast) — the standard library's AST design; the marker-method pattern comes from here.

---

Next: [02-order-by-nulls.md](02-order-by-nulls.md)
