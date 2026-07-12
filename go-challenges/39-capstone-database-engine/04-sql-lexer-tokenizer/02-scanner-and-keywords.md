# Exercise 2: The Single-Pass Scanner and Keyword Recognition

This exercise builds the lexer itself: the two-offset cursor, the `NextToken` dispatch, and the keyword table that turns any alphabetic run into either an identifier or a canonicalized keyword. It is the spine every later exercise extends, so it is assembled here in full — token model, keyword map, and scanner — and tested against the parts that define the scanner's character: case-insensitive keywords, greedy identifiers, maximal-munch operators, single-byte punctuation, and exact line/column tracking.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             TokenType, the token constant block, IsKeyword, Token
keywords.go          the keyword map and lookupIdent
lexer.go             Lexer, New, readChar, peekChar, NextToken, Tokenize, the readers
cmd/
  demo/
    main.go          tokenize a statement and print each token's line:col and literal
lexer_test.go        keyword case-folding, identifiers, operators, punctuation, position
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `cmd/demo/main.go`, `lexer_test.go`.
- Implement: `Lexer` with `New`, `readChar`, `peekChar`, `skipWhitespace`, `NextToken`, and the package function `Tokenize`, plus the keyword map and `lookupIdent`.
- Test: `lexer_test.go` covers case-insensitive keyword recognition, identifier scanning, the one- and two-character operators, punctuation, and 1-based line/column tracking across newlines.
- Verify: `go test -run 'TestTokenize|TestLine' -race ./...`

### Why a two-offset cursor and a keyword map, not a per-character branch

The scanner reads the source exactly once, left to right, holding two byte offsets: `pos`, the index of the current byte `ch`, and `readPos`, the index of the next byte to read. `readChar` slides the window forward by one; `peekChar` returns the next byte without moving. That single byte of lookahead is all the scanner ever needs, because every token is decided by the current byte plus at most one peek: `-` versus `--`, `/` versus `/*`, `<` versus `<=`, a fractional dot versus a punctuation dot. Keeping lookahead at exactly one byte is what makes `NextToken` a tight, allocation-free inner loop.

`NextToken` is a dispatch on the current byte. It first skips whitespace, snapshots the start position so the emitted token points at its first byte, then branches: comment leads, string and quoted-identifier openers, letters, digits, the two-character operator forms, and finally a `default` that handles single-byte operators and punctuation. The two-character forms (`<>`, `<=`, `>=`, `!=`) are tested *before* the single-byte ones because of maximal munch: `>=` must be one token, not `>` followed by `=`. This ordering is the whole of the longest-match rule for operators.

Keyword recognition is deliberately *not* part of the scan loop. The loop treats any run of letters, digits, and underscores uniformly as an identifier; only after the run is read does `lookupIdent` fold it to upper case once and look it up in a `map[string]TokenType` built at package init. A keyword match returns the keyword type with the canonical uppercase spelling in `Literal`; a miss returns `TokenIdent` with the raw bytes. This concentrates all keyword knowledge in one table instead of smearing it across dozens of character comparisons in the hot loop, and it makes the case-insensitivity of SQL keywords a property of one `strings.ToUpper` call rather than of the scanner's control flow.

Position tracking lives entirely in `readChar`: every consumed byte increments `col`, and a consumed `\n` increments `line` and resets `col` to 0 (so the next `readChar` makes the first column of the new line 1). Because `New` calls `readChar` once to prime `ch`, the first real byte lands at line 1, column 1.

Create `token.go`:

```go
package lexer

import "fmt"

// TokenType identifies the category of a SQL token.
type TokenType int

const (
	// Special tokens.
	TokenIllegal TokenType = iota
	TokenError
	TokenEOF

	// Literals.
	TokenIdent  // unquoted identifier
	TokenQIdent // double-quoted identifier
	TokenInt    // integer literal: 42
	TokenFloat  // float literal: 3.14, 1.5e10
	TokenString // single-quoted string literal

	// Keywords — sentinels bracket the keyword block for IsKeyword.
	keywordStart
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
	keywordEnd

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
	TokenColon     // :

	// Comments — only emitted when KeepComments is true.
	TokenLineComment  // -- ...
	TokenBlockComment // /* ... */
)

// IsKeyword reports whether t is a SQL keyword.
func (t TokenType) IsKeyword() bool {
	return t > keywordStart && t < keywordEnd
}

// Token is a single lexical unit in a SQL source string.
type Token struct {
	Type    TokenType
	Literal string // raw text for identifiers/literals; canonical uppercase for keywords
	Pos     int    // byte offset of the first character in the source
	Line    int    // 1-based line number
	Col     int    // 1-based column number
}

// String returns a debug representation.
func (tok Token) String() string {
	return fmt.Sprintf("Token(%d, %q, %d:%d)", tok.Type, tok.Literal, tok.Line, tok.Col)
}
```

Create `keywords.go`:

```go
package lexer

import "strings"

// keywords maps the canonical uppercase spelling of each SQL keyword to its
// token type. Built once at package init; never mutated after that.
var keywords map[string]TokenType

func init() {
	keywords = map[string]TokenType{
		"SELECT":   TokenSelect,
		"FROM":     TokenFrom,
		"WHERE":    TokenWhere,
		"INSERT":   TokenInsert,
		"INTO":     TokenInto,
		"VALUES":   TokenValues,
		"UPDATE":   TokenUpdate,
		"SET":      TokenSet,
		"DELETE":   TokenDelete,
		"CREATE":   TokenCreate,
		"TABLE":    TokenTable,
		"DROP":     TokenDrop,
		"INDEX":    TokenIndex,
		"ON":       TokenOn,
		"AND":      TokenAnd,
		"OR":       TokenOr,
		"NOT":      TokenNot,
		"NULL":     TokenNull,
		"TRUE":     TokenTrue,
		"FALSE":    TokenFalse,
		"ORDER":    TokenOrder,
		"BY":       TokenBy,
		"ASC":      TokenAsc,
		"DESC":     TokenDesc,
		"LIMIT":    TokenLimit,
		"OFFSET":   TokenOffset,
		"JOIN":     TokenJoin,
		"LEFT":     TokenLeft,
		"RIGHT":    TokenRight,
		"INNER":    TokenInner,
		"OUTER":    TokenOuter,
		"GROUP":    TokenGroup,
		"HAVING":   TokenHaving,
		"AS":       TokenAs,
		"DISTINCT": TokenDistinct,
		"COUNT":    TokenCount,
		"SUM":      TokenSum,
		"AVG":      TokenAvg,
		"MIN":      TokenMin,
		"MAX":      TokenMax,
		"IN":       TokenIn,
		"BETWEEN":  TokenBetween,
		"LIKE":     TokenLike,
		"IS":       TokenIs,
		"EXISTS":   TokenExists,
		"PRIMARY":  TokenPrimary,
		"KEY":      TokenKey,
		"INTEGER":  TokenInteger,
		"TEXT":     TokenText,
		"REAL":     TokenReal,
		"BOOLEAN":  TokenBoolean,
		"BEGIN":    TokenBegin,
		"COMMIT":   TokenCommit,
		"ROLLBACK": TokenRollback,
	}
}

// lookupIdent returns the keyword token type when ident is a SQL keyword
// (comparison is case-insensitive), or TokenIdent for a plain identifier.
func lookupIdent(ident string) TokenType {
	if tt, ok := keywords[strings.ToUpper(ident)]; ok {
		return tt
	}
	return TokenIdent
}
```

Create `lexer.go`:

```go
package lexer

import (
	"fmt"
	"strings"
)

// Lexer tokenizes a SQL source string. Create one with New; call NextToken
// repeatedly until it returns a Token with Type == TokenEOF or TokenError.
type Lexer struct {
	input   string
	pos     int  // byte index of ch (the current byte)
	readPos int  // byte index of the next byte to consume
	ch      byte // the byte at pos; 0 at EOF

	line int // 1-based line number of ch
	col  int // 1-based column number of ch

	// KeepComments, when true, causes NextToken to emit TokenLineComment and
	// TokenBlockComment tokens instead of silently discarding comments.
	KeepComments bool
}

// New creates a Lexer for input and reads the first byte.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1, col: 0}
	l.readChar()
	return l
}

// readChar advances the lexer by one byte.
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

// peekChar returns the next byte without consuming it. Returns 0 at EOF.
func (l *Lexer) peekChar() byte {
	if l.readPos >= len(l.input) {
		return 0
	}
	return l.input[l.readPos]
}

// skipWhitespace consumes ASCII whitespace without emitting a token.
func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\r' || l.ch == '\n' {
		l.readChar()
	}
}

// NextToken returns the next token in the SQL source. After TokenEOF or
// TokenError is returned, all subsequent calls also return TokenEOF.
func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	startLine := l.line
	startCol := l.col
	startPos := l.pos

	switch {
	case l.ch == 0:
		return Token{Type: TokenEOF, Literal: "", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '-' && l.peekChar() == '-':
		return l.readLineComment(startPos, startLine, startCol)

	case l.ch == '/' && l.peekChar() == '*':
		return l.readBlockComment(startPos, startLine, startCol)

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

// readSingle returns a token for the current single-byte operator or
// punctuation character, without advancing the lexer. The caller advances.
func (l *Lexer) readSingle(pos, line, col int) Token {
	ch := l.ch
	var tt TokenType
	switch ch {
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
	case ':':
		tt = TokenColon
	default:
		return Token{
			Type:    TokenError,
			Literal: fmt.Sprintf("unexpected character %q at %d:%d", ch, line, col),
			Pos:     pos,
			Line:    line,
			Col:     col,
		}
	}
	return Token{Type: tt, Literal: string(ch), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readLineComment(pos, line, col int) Token {
	l.readChar() // consume first -
	l.readChar() // consume second -
	start := l.pos
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	text := strings.TrimSpace(l.input[start:l.pos])
	if l.KeepComments {
		return Token{Type: TokenLineComment, Literal: text, Pos: pos, Line: line, Col: col}
	}
	return l.NextToken()
}

func (l *Lexer) readBlockComment(pos, line, col int) Token {
	l.readChar() // consume /
	l.readChar() // consume *
	depth := 1
	var buf strings.Builder
	for depth > 0 {
		if l.ch == 0 {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated block comment starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '/' && l.peekChar() == '*' {
			depth++
			buf.WriteByte(l.ch)
			l.readChar()
			buf.WriteByte(l.ch)
			l.readChar()
			continue
		}
		if l.ch == '*' && l.peekChar() == '/' {
			depth--
			if depth == 0 {
				l.readChar() // consume *
				l.readChar() // consume /
				break
			}
			buf.WriteByte(l.ch)
			l.readChar()
			buf.WriteByte(l.ch)
			l.readChar()
			continue
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	if l.KeepComments {
		return Token{Type: TokenBlockComment, Literal: buf.String(), Pos: pos, Line: line, Col: col}
	}
	return l.NextToken()
}

func (l *Lexer) readString(pos, line, col int) Token {
	l.readChar() // consume opening '
	var buf strings.Builder
	for {
		if l.ch == 0 {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated string literal starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '\'' {
			if l.peekChar() == '\'' {
				// SQL standard escape: '' -> one '
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
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated quoted identifier starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '"' {
			if l.peekChar() == '"' {
				// SQL standard escape: "" -> one "
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
		// Keyword: the Literal field holds the canonical uppercase spelling.
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
	// Optional fractional part: only when the '.' is followed by a digit,
	// so that a trailing dot (e.g., "table.col") is not swallowed.
	if l.ch == '.' && isDigit(l.peekChar()) {
		tt = TokenFloat
		l.readChar() // consume .
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	// Optional exponent.
	if l.ch == 'e' || l.ch == 'E' {
		tt = TokenFloat
		l.readChar() // consume e/E
		if l.ch == '+' || l.ch == '-' {
			l.readChar()
		}
		if !isDigit(l.ch) {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("malformed numeric literal at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	return Token{Type: tt, Literal: l.input[start:l.pos], Pos: pos, Line: line, Col: col}
}

// Tokenize is a convenience wrapper that returns all tokens for input,
// ending with a single TokenEOF (or TokenError on failure). Useful in
// tests and demos.
func Tokenize(input string) []Token {
	l := New(input)
	var tokens []Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF || tok.Type == TokenError {
			break
		}
	}
	return tokens
}

func isLetter(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}
```

The whole baseline ships here even though this exercise's tests exercise only the scanner core. The reason is independence: each exercise in this lesson is its own module, so the string, number, and comment readers travel with the scanner rather than being introduced piecemeal — later exercises lean on the exact same `lexer.go` and only add new files beside it. The scanner is byte-based (`byte`, not `rune`) on purpose: SQL identifiers and keywords are ASCII, and string-literal content is copied byte-for-byte and handed to the application layer, which owns encoding interpretation.

### The runnable demo

The demo tokenizes a short, single-line statement and prints each token's 1-based `line:col` next to its literal, so the position tracking and the keyword canonicalization are both visible at a glance.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/scanner-and-keywords"
)

func main() {
	input := "SELECT id FROM users WHERE id = 1;"
	for _, tok := range lexer.Tokenize(input) {
		if tok.Type == lexer.TokenEOF {
			break
		}
		fmt.Printf("%d:%-2d  %s\n", tok.Line, tok.Col, tok.Literal)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1:1   SELECT
1:8   id
1:11  FROM
1:16  users
1:22  WHERE
1:28  id
1:31  =
1:33  1
1:34  ;
```

The keywords come back uppercased (`SELECT`, `FROM`, `WHERE`) even when the source spells them otherwise, while `id` and `users` keep their original bytes; the `line:col` of each token points at its first character.

### Tests

The tests pin the scanner's defining behaviors. `TestTokenizeKeywordCaseInsensitive` proves `SELECT`/`select`/`Select` all map to `TokenSelect` with an uppercase literal; `TestTokenizeIdentifier` proves a bareword survives byte-for-byte; `TestTokenizeOperators` and `TestTokenizePunctuation` walk every operator and punctuation form, with the two-character operators confirming maximal munch; and `TestLineAndColumnTracking` checks that newlines advance `line` and reset `col`.

Create `lexer_test.go`:

```go
package lexer

import (
	"strings"
	"testing"
)

func TestTokenizeKeywordCaseInsensitive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  TokenType
	}{
		{"SELECT", TokenSelect},
		{"select", TokenSelect},
		{"Select", TokenSelect},
		{"FROM", TokenFrom},
		{"WHERE", TokenWhere},
		{"INSERT", TokenInsert},
		{"VALUES", TokenValues},
		{"UPDATE", TokenUpdate},
		{"DELETE", TokenDelete},
		{"CREATE", TokenCreate},
		{"AND", TokenAnd},
		{"OR", TokenOr},
		{"NOT", TokenNot},
		{"NULL", TokenNull},
		{"ORDER", TokenOrder},
		{"LIMIT", TokenLimit},
		{"JOIN", TokenJoin},
		{"ROLLBACK", TokenRollback},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if len(toks) < 1 || toks[0].Type != tc.want {
				t.Fatalf("Tokenize(%q)[0].Type = %v, want %v", tc.input, toks[0].Type, tc.want)
			}
			if toks[0].Literal != strings.ToUpper(tc.input) {
				t.Fatalf("Tokenize(%q)[0].Literal = %q, want %q",
					tc.input, toks[0].Literal, strings.ToUpper(tc.input))
			}
		})
	}
}

func TestTokenizeIdentifier(t *testing.T) {
	t.Parallel()

	toks := Tokenize("my_column")
	if toks[0].Type != TokenIdent || toks[0].Literal != "my_column" {
		t.Fatalf("got %v", toks[0])
	}
}

func TestTokenizeOperators(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  TokenType
		lit   string
	}{
		{"+", TokenPlus, "+"},
		{"-", TokenMinus, "-"},
		{"*", TokenAsterisk, "*"},
		{"/", TokenSlash, "/"},
		{"=", TokenEq, "="},
		{"!=", TokenNeq, "!="},
		{"<>", TokenNeq, "<>"},
		{"<", TokenLt, "<"},
		{">", TokenGt, ">"},
		{"<=", TokenLtEq, "<="},
		{">=", TokenGtEq, ">="},
	}
	for _, tc := range cases {
		t.Run(tc.lit, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if toks[0].Type != tc.want || toks[0].Literal != tc.lit {
				t.Fatalf("got Type=%v Literal=%q, want %v %q",
					toks[0].Type, toks[0].Literal, tc.want, tc.lit)
			}
		})
	}
}

func TestTokenizePunctuation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  TokenType
	}{
		{"(", TokenLParen},
		{")", TokenRParen},
		{",", TokenComma},
		{";", TokenSemicolon},
		{".", TokenDot},
		{":", TokenColon},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if toks[0].Type != tc.want {
				t.Fatalf("Tokenize(%q): got %v, want %v", tc.input, toks[0].Type, tc.want)
			}
		})
	}
}

func TestLineAndColumnTracking(t *testing.T) {
	t.Parallel()

	// SELECT is on line 1 col 1; id is on line 2 col 3;
	// FROM is on line 3 col 1; users is on line 4 col 3.
	input := "SELECT\n  id\nFROM\n  users"
	toks := Tokenize(input)
	wants := []struct{ line, col int }{
		{1, 1},
		{2, 3},
		{3, 1},
		{4, 3},
	}
	for i, w := range wants {
		if toks[i].Line != w.line || toks[i].Col != w.col {
			t.Errorf("toks[%d] %q: got %d:%d, want %d:%d",
				i, toks[i].Literal, toks[i].Line, toks[i].Col, w.line, w.col)
		}
	}
}

func TestFullStatement(t *testing.T) {
	t.Parallel()

	input := `SELECT id, name FROM users WHERE status = 'active' AND age >= 18;`
	want := []TokenType{
		TokenSelect, TokenIdent, TokenComma, TokenIdent,
		TokenFrom, TokenIdent,
		TokenWhere, TokenIdent, TokenEq, TokenString,
		TokenAnd, TokenIdent, TokenGtEq, TokenInt,
		TokenSemicolon, TokenEOF,
	}
	toks := Tokenize(input)
	if len(toks) != len(want) {
		t.Fatalf("len(toks) = %d, want %d\ntokens: %v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].Type != w {
			t.Errorf("toks[%d]: got %v, want %v (literal=%q)",
				i, toks[i].Type, w, toks[i].Literal)
		}
	}
}
```

## Review

The scanner is correct when maximal munch holds and position never drifts. Confirm `>=`, `<=`, `<>`, and `!=` each come back as one two-character token rather than two single-character ones, that an identifier runs to the first byte that is not a letter, digit, or underscore, and that every keyword regardless of source case returns its keyword type with an uppercase `Literal` while barewords keep their bytes. The `line:col` of each token must point at its first character, with newlines advancing the line and resetting the column, so a multi-line statement reports positions an editor would agree with.

Two mistakes are worth naming. Testing the single-character operator branch before the two-character ones breaks longest match — `>=` becomes `>` then `=` — so the two-byte forms must be checked first. And folding keyword recognition into the scan loop, comparing `l.ch` against the letters of `SELECT` byte by byte, both breaks case-insensitivity and balloons the hot loop; lexing a uniform identifier run and doing one `strings.ToUpper` plus one map lookup keeps the loop tight and the keyword table the single source of truth.

## Resources

- [Go Specification: Lexical elements](https://go.dev/ref/spec#Lexical_elements) — the language-level lexical grammar (tokens, identifiers, literals) the toolchain scanner realizes with the same two-offset cursor model.
- [pkg.go.dev: go/scanner](https://pkg.go.dev/go/scanner) — the standard library's Go-source scanner, whose `offset`/`rdOffset` cursor layout is the reference for the `pos`/`readPos` pair used here.
- [PostgreSQL: Lexical Structure](https://www.postgresql.org/docs/current/sql-syntax-lexical.html) — authoritative definition of SQL identifiers, keywords, and operators.
- [pkg.go.dev: strings package](https://pkg.go.dev/strings) — `strings.ToUpper` and `strings.Builder`, used in keyword folding and the literal readers.

---

Back to [01-token-types.md](01-token-types.md) | Next: [03-strings-and-identifiers.md](03-strings-and-identifiers.md)
