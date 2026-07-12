# Exercise 4: CREATE INDEX and ALTER TABLE ADD COLUMN

The core parser covers `CREATE TABLE` and `DROP TABLE` but no other DDL. This exercise adds two common statements through an independent entry point, `ParseDDL`, so the new grammar stays isolated from the core `ParseStatement`. `INDEX`, `ON`, and `TABLE` are lexer keywords; `UNIQUE`, `ALTER`, `ADD`, and `COLUMN` are not, so they arrive as `TokenIdent` and are matched by uppercased literal. The `ALTER TABLE ... ADD COLUMN` path reuses the bundled `parseColumnDef`, so a column constraint such as `NOT NULL` or `DEFAULT` parses with no extra code.

This module is fully self-contained. It bundles its own minimal lexer and the core parser, depends on nothing but the standard library, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sqlddl/
  go.mod
  lexer/
    lexer.go          bundled minimal SQL lexer
  ast.go              bundled AST node types (incl. ColumnDef, reused here)
  parser.go           bundled core parser (incl. parseColumnDef, reused here)
  ddl_ext.go          CreateIndexStatement, AlterTableStatement, ParseDDL
  ddl_ext_test.go     create index, alter add column, round trip, errors
  cmd/
    demo/
      main.go         parse a few DDL statements and reprint them
```

- Files: `lexer/lexer.go`, `ast.go`, `parser.go`, `ddl_ext.go`, `ddl_ext_test.go`, `cmd/demo/main.go`.
- Implement: `CreateIndexStatement` and `AlterTableStatement` (each with `String()`), `ParseDDL`, the unexported `parseCreateIndex` / `parseAlterTable`, and `ErrUnsupportedDDL`.
- Test: `CREATE [UNIQUE] INDEX [IF NOT EXISTS]`, multi-column indexes, `ALTER TABLE ... ADD [COLUMN]` with the optional keyword, round trip, the unsupported-statement sentinel, and malformed-input errors.
- Verify: `go test -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/05-sql-parser/04-create-index-alter-table/lexer go-solutions/39-capstone-database-engine/05-sql-parser/04-create-index-alter-table/cmd/demo && cd go-solutions/39-capstone-database-engine/05-sql-parser/04-create-index-alter-table
```

### A separate entry point, and reuse where it counts

`ParseDDL` is deliberately not wired into the core `ParseStatement`. Keeping it a separate function means the extension grammar — two statements that the core does not know about — never perturbs the main dispatch, and a caller opts into it explicitly. A statement `ParseDDL` does not recognize yields `ErrUnsupportedDDL` (so a caller can fall back to the core parser), while a malformed instance of a form it does handle yields `ErrSyntax`. Two sentinels, two different recovery strategies.

The interesting reuse is in `ALTER TABLE ... ADD COLUMN`: after consuming the optional `COLUMN` keyword, it calls the bundled `parseColumnDef`, the same function the core uses inside `CREATE TABLE`. That single call buys the entire column-constraint grammar — type, `NOT NULL`, `PRIMARY KEY`, `DEFAULT expr` — for free, including the Pratt-parsed default expression. This is the dividend of building the core as composable methods on one `Parser`: a new statement that needs a column definition does not re-describe what a column is.

Contextual keywords carry the rest. `UNIQUE`, `ALTER`, `ADD`, and `COLUMN` are matched as uppercased identifier literals exactly where the grammar expects them, the same `IF`/`DEFAULT` technique from the core. `IF NOT EXISTS` reuses the two-token `IF` + `NOT` + `EXISTS` recognition. And `CREATE INDEX` requires at least one column, so an empty parenthesized list is a syntax error rather than a silently empty index.

The module bundles the same minimal lexer and core parser as Exercise 1 so it stands alone — the only change is the module path in the import. They are reproduced in full below; skim them, then focus on the feature file that follows.

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

Create `parser.go`:

```go
package parser

import (
	"errors"
	"fmt"
	"strings"

	"example.com/sqlddl/lexer"
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

Now the feature. `ParseDDL` dispatches on `CREATE` versus a literal `ALTER`, runs the matching parser, then requires clean end-of-input (after an optional trailing semicolon). `parseAlterTable` reuses `parseColumnDef` for the added column.

Create `ddl_ext.go`:

```go
package parser

import (
	"errors"
	"fmt"
	"strings"

	"example.com/sqlddl/lexer"
)

// ErrUnsupportedDDL is returned by ParseDDL for a statement it does not handle.
// Test with: errors.Is(err, ErrUnsupportedDDL).
var ErrUnsupportedDDL = errors.New("unsupported DDL statement")

// CreateIndexStatement covers CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON
// table (col, …).
type CreateIndexStatement struct {
	Name        string
	Unique      bool
	IfNotExists bool
	Table       string
	Columns     []string
}

func (*CreateIndexStatement) stmtNode() {}

func (s *CreateIndexStatement) String() string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if s.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if s.IfNotExists {
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteString(s.Name)
	b.WriteString(" ON ")
	b.WriteString(s.Table)
	b.WriteString(" (")
	b.WriteString(strings.Join(s.Columns, ", "))
	b.WriteByte(')')
	return b.String()
}

// AlterTableStatement covers ALTER TABLE name ADD [COLUMN] coldef.
type AlterTableStatement struct {
	Table  string
	AddCol *ColumnDef
}

func (*AlterTableStatement) stmtNode() {}

func (s *AlterTableStatement) String() string {
	return "ALTER TABLE " + s.Table + " ADD COLUMN " + s.AddCol.String()
}

// ParseDDL parses one CREATE INDEX or ALTER TABLE ADD COLUMN statement. A
// statement it does not recognize yields ErrUnsupportedDDL; a malformed one of
// the supported forms yields a *ParseError wrapping ErrSyntax.
func ParseDDL(sql string) (Statement, error) {
	p := New(lexer.New(sql))

	var stmt Statement
	switch {
	case p.curIs(lexer.TokenCreate):
		stmt = p.parseCreateIndex()
	case p.curIs(lexer.TokenIdent) && strings.ToUpper(p.cur.Literal) == "ALTER":
		stmt = p.parseAlterTable()
	default:
		return nil, fmt.Errorf("line %d:%d: %q: %w",
			p.cur.Line, p.cur.Col, p.cur.Literal, ErrUnsupportedDDL)
	}

	if err := p.firstErr(); err != nil {
		return nil, err
	}
	if p.curIs(lexer.TokenSemicolon) {
		p.nextToken()
	}
	if !p.curIs(lexer.TokenEOF) {
		p.syntaxErr("unexpected trailing token %q", p.cur.Literal)
		return nil, p.firstErr()
	}
	return stmt, nil
}

func (p *Parser) parseCreateIndex() Statement {
	p.nextToken() // consume CREATE
	stmt := &CreateIndexStatement{}

	if p.curIs(lexer.TokenIdent) && strings.ToUpper(p.cur.Literal) == "UNIQUE" {
		stmt.Unique = true
		p.nextToken()
	}
	if !p.curIs(lexer.TokenIndex) {
		p.syntaxErr("expected INDEX, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume INDEX

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

	if !p.curIs(lexer.TokenOn) {
		p.syntaxErr("expected ON after index name, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume ON
	stmt.Table = p.cur.Literal
	p.nextToken()

	if !p.expect(lexer.TokenLParen) {
		return nil
	}
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
	if len(stmt.Columns) == 0 {
		p.syntaxErr("CREATE INDEX requires at least one column")
		return nil
	}
	return stmt
}

func (p *Parser) parseAlterTable() Statement {
	p.nextToken() // consume ALTER
	if !p.curIs(lexer.TokenTable) {
		p.syntaxErr("expected TABLE after ALTER, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume TABLE
	stmt := &AlterTableStatement{Table: p.cur.Literal}
	p.nextToken()

	if !(p.curIs(lexer.TokenIdent) && strings.ToUpper(p.cur.Literal) == "ADD") {
		p.syntaxErr("expected ADD, got %q", p.cur.Literal)
		return nil
	}
	p.nextToken() // consume ADD
	if p.curIs(lexer.TokenIdent) && strings.ToUpper(p.cur.Literal) == "COLUMN" {
		p.nextToken() // optional COLUMN
	}

	col := p.parseColumnDef()
	if col == nil {
		return nil
	}
	stmt.AddCol = col
	return stmt
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	parser "example.com/sqlddl"
)

func main() {
	stmts := []string{
		"CREATE UNIQUE INDEX idx_email ON users (email)",
		"CREATE INDEX IF NOT EXISTS idx_name ON users (last, first)",
		"ALTER TABLE users ADD COLUMN age INTEGER NOT NULL",
		"ALTER TABLE t ADD flag BOOLEAN DEFAULT TRUE",
	}
	for _, sql := range stmts {
		stmt, err := parser.ParseDDL(sql)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}
		fmt.Println(stmt.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (note the printer canonicalizes `ADD` to `ADD COLUMN`, so the last line gains the optional keyword it omitted on input):

```
CREATE UNIQUE INDEX idx_email ON users (email)
CREATE INDEX IF NOT EXISTS idx_name ON users (last, first)
ALTER TABLE users ADD COLUMN age INTEGER NOT NULL
ALTER TABLE t ADD COLUMN flag BOOLEAN DEFAULT TRUE
```

Create `ddl_ext_test.go`:

```go
package parser

import (
	"errors"
	"testing"
)

func TestParseCreateIndex(t *testing.T) {
	t.Parallel()

	stmt, err := ParseDDL("CREATE UNIQUE INDEX idx_email ON users (email)")
	if err != nil {
		t.Fatalf("ParseDDL error: %v", err)
	}
	ci, ok := stmt.(*CreateIndexStatement)
	if !ok {
		t.Fatalf("got %T, want *CreateIndexStatement", stmt)
	}
	if !ci.Unique {
		t.Fatal("Unique should be true")
	}
	if ci.Name != "idx_email" || ci.Table != "users" {
		t.Fatalf("index = %+v", ci)
	}
	if len(ci.Columns) != 1 || ci.Columns[0] != "email" {
		t.Fatalf("Columns = %v, want [email]", ci.Columns)
	}
}

func TestParseCreateIndexMultiColumn(t *testing.T) {
	t.Parallel()

	stmt, err := ParseDDL("CREATE INDEX idx ON t (a, b, c)")
	if err != nil {
		t.Fatalf("ParseDDL error: %v", err)
	}
	ci := stmt.(*CreateIndexStatement)
	if ci.Unique {
		t.Fatal("Unique should be false")
	}
	if len(ci.Columns) != 3 {
		t.Fatalf("len(Columns) = %d, want 3", len(ci.Columns))
	}
}

func TestParseCreateIndexIfNotExists(t *testing.T) {
	t.Parallel()

	stmt, err := ParseDDL("CREATE INDEX IF NOT EXISTS idx ON t (a)")
	if err != nil {
		t.Fatalf("ParseDDL error: %v", err)
	}
	if !stmt.(*CreateIndexStatement).IfNotExists {
		t.Fatal("IfNotExists should be true")
	}
}

func TestParseAlterTableAddColumn(t *testing.T) {
	t.Parallel()

	stmt, err := ParseDDL("ALTER TABLE users ADD COLUMN age INTEGER NOT NULL")
	if err != nil {
		t.Fatalf("ParseDDL error: %v", err)
	}
	at, ok := stmt.(*AlterTableStatement)
	if !ok {
		t.Fatalf("got %T, want *AlterTableStatement", stmt)
	}
	if at.Table != "users" {
		t.Fatalf("Table = %q, want users", at.Table)
	}
	if at.AddCol == nil {
		t.Fatal("AddCol should not be nil")
	}
	if at.AddCol.Name != "age" || at.AddCol.Type != ColTypeInteger {
		t.Fatalf("AddCol = %+v", at.AddCol)
	}
	if !at.AddCol.NotNull {
		t.Fatal("AddCol should be NOT NULL")
	}
}

func TestParseAlterTableAddColumnNoColumnKeyword(t *testing.T) {
	t.Parallel()

	// COLUMN is optional.
	stmt, err := ParseDDL("ALTER TABLE t ADD note TEXT")
	if err != nil {
		t.Fatalf("ParseDDL error: %v", err)
	}
	at := stmt.(*AlterTableStatement)
	if at.AddCol.Name != "note" || at.AddCol.Type != ColTypeText {
		t.Fatalf("AddCol = %+v", at.AddCol)
	}
}

func TestDDLRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []string{
		"CREATE INDEX idx ON t (a)",
		"CREATE UNIQUE INDEX idx_email ON users (email)",
		"CREATE INDEX IF NOT EXISTS idx ON t (a, b)",
		"ALTER TABLE users ADD COLUMN age INTEGER NOT NULL",
		"ALTER TABLE t ADD COLUMN flag BOOLEAN DEFAULT TRUE",
	}
	for _, want := range cases {
		stmt, err := ParseDDL(want)
		if err != nil {
			t.Errorf("ParseDDL(%q): %v", want, err)
			continue
		}
		got := stmt.String()
		stmt2, err2 := ParseDDL(got)
		if err2 != nil {
			t.Errorf("re-parse(%q): %v", got, err2)
			continue
		}
		if stmt2.String() != got {
			t.Errorf("round-trip mismatch: first %q, second %q", got, stmt2.String())
		}
	}
}

func TestParseDDLErrors(t *testing.T) {
	t.Parallel()

	t.Run("unsupported wraps ErrUnsupportedDDL", func(t *testing.T) {
		t.Parallel()
		_, err := ParseDDL("SELECT 1 FROM t")
		if !errors.Is(err, ErrUnsupportedDDL) {
			t.Fatalf("err = %v, want ErrUnsupportedDDL", err)
		}
	})

	cases := []struct {
		name string
		sql  string
	}{
		{"create missing INDEX", "CREATE TABLE t (a INTEGER)"},
		{"index missing ON", "CREATE INDEX idx t (a)"},
		{"index empty columns", "CREATE INDEX idx ON t ()"},
		{"alter missing TABLE", "ALTER INDEX t ADD COLUMN a INTEGER"},
		{"alter missing ADD", "ALTER TABLE t DROP COLUMN a"},
		{"alter bad type", "ALTER TABLE t ADD COLUMN a FOO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseDDL(tc.sql)
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("ParseDDL(%q): err = %v, want ErrSyntax", tc.sql, err)
			}
		})
	}
}
```

## Review

The DDL parser is correct when both statements round-trip and the two error sentinels stay distinct. `CREATE UNIQUE INDEX ... ON ... (cols)` must capture the unique flag, name, table, and every column; `IF NOT EXISTS` must be recognized; and a multi-column list must keep its order. `ALTER TABLE ... ADD [COLUMN] coldef` must accept the optional `COLUMN` keyword and parse the column through the shared `parseColumnDef`, so `age INTEGER NOT NULL` carries its constraint. Every supported form must survive a print/re-parse cycle. An unrelated statement must satisfy `errors.Is(err, ErrUnsupportedDDL)`, while malformed instances (`CREATE INDEX idx t (a)` with no `ON`, an empty column list, `ALTER INDEX ...`, an unknown column type) must satisfy `errors.Is(err, ErrSyntax)`.

The trap is re-describing a column definition by hand instead of calling `parseColumnDef`. The whole point of the shared method is that the constraint grammar lives in exactly one place; duplicating it would let `ALTER TABLE` and `CREATE TABLE` drift apart.

## Resources

- [SQLite: CREATE INDEX](https://www.sqlite.org/lang_createindex.html) — the `CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON table (cols)` grammar.
- [SQLite: ALTER TABLE](https://www.sqlite.org/lang_altertable.html) — the `ADD [COLUMN]` form, including the optional keyword.
- [PostgreSQL: ALTER TABLE](https://www.postgresql.org/docs/current/sql-altertable.html) — a second dialect's `ADD COLUMN` with full column constraints.

---

Back to [03-exists-predicates.md](03-exists-predicates.md)
