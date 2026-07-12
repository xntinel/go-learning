# Exercise 5: Bind Parameters and Dollar-Quoted Strings

Real SQL drivers carry constructs the base lexer does not yet recognize: PostgreSQL numbered placeholders (`$1`), the positional `?`, named `:name` forms, the cast operator `::`, and dollar-quoted strings (`$$text$$`, `$tag$text$tag$`) that sidestep quote-doubling entirely. This exercise adds a standalone `ScanParameter` for those forms. It is purely additive: a fresh `const` block introduces new token types at a `+1000` offset without touching the baseline `iota` block, so the existing token set stays byte-for-byte unchanged and the base API keeps compiling.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             the baseline token model (unchanged)
keywords.go          the baseline keyword map (unchanged)
lexer.go             the baseline lexer (unchanged)
params.go            TokenPositional/Numbered/Named/DollarString/Cast, ScanParameter
cmd/
  demo/
    main.go          scan each parameter form and print its literal and end offset
params_test.go       every form, the cast operator, and the sentinel-error cases
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `params.go`, `cmd/demo/main.go`, `params_test.go`.
- Implement: the offset `const` block (`TokenPositional`, `TokenNumbered`, `TokenNamed`, `TokenDollarString`, `TokenCast`), the sentinel errors, `ScanParameter`, and `scanDollarString`.
- Test: `params_test.go` covers `?`, `$1`, `::`, `:name`, both dollar-quote forms, scanning at an offset, and the `ErrNotParameter`/`ErrEmptyNamedParam`/`ErrUnterminatedDollar` errors.
- Verify: `go test -run 'TestScanParameter|ExampleScanParameter' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/05-bind-parameters/cmd/demo && cd go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/05-bind-parameters
```

### Why an additive const block and a separate scanner

The base token constants in `token.go` are produced by one `iota` run, and any code that switched on them — the parser in the next lesson included — is sensitive to their exact values. So the new parameter tokens cannot be spliced into that block without renumbering everything after the splice point. The idiomatic Go answer is a fresh `const` block whose first line is `TokenPositional TokenType = iota + 1000`: a new block restarts `iota` at 0, and the `+1000` offset lifts the whole group clear of the base set, which has well under a hundred members. The base tokens keep their values, the parameter tokens get their own contiguous range, and the two never collide. This is the same additive discipline a real engine uses to extend a token enum across versions without breaking on-disk or cross-module assumptions.

`ScanParameter` is a position-based function, `func(input string, pos int) (Token, int, error)`, rather than a method on `Lexer`, because these constructs are dialect extensions a driver layers on top of the core scan: it returns the token plus the offset just past it, so a caller can splice parameter scanning into its own loop. The dispatch is a switch on `input[pos]`. `?` is the whole token. A `:` needs one byte of lookahead for maximal munch — `::` is the cast operator and must be tested before the `:name` branch, exactly the same longest-match reasoning that puts `>=` before `>` in the core scanner. A `$` is ambiguous until the next byte: a digit makes it a numbered placeholder (`$1`), anything else opens a dollar-quoted string. Every unrecognized lead byte returns `ErrNotParameter`, a sentinel the caller matches with `errors.Is`.

Dollar-quoted strings are PostgreSQL's escape-free string form: the opening delimiter is `$tag$` (an empty tag gives `$$`), and the content runs verbatim until the next identical delimiter, with no doubling and no backslash processing. `scanDollarString` reads the tag, forms the delimiter, then uses `strings.Index` to find the matching close; a missing close is `ErrUnterminatedDollar`. The returned `Literal` is the content between delimiters, tag stripped — `$$hello$$` yields `hello` and `$t$a$b$t$` yields `a$b`, the inner `$b$` surviving because it does not match the `$t$` delimiter.

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

Create `params.go`:

```go
package lexer

import (
	"errors"
	"fmt"
	"strings"
)

// Parameter, cast, and dollar-quote token types. A fresh const block restarts
// iota at 0, so the +1000 offset guarantees these values never collide with the
// base tokens in token.go. The base token set stays byte-for-byte unchanged.
const (
	TokenPositional   TokenType = iota + 1000 // ?              positional placeholder
	TokenNumbered                             // $1, $42        numbered placeholder
	TokenNamed                                // :name          named placeholder
	TokenDollarString                         // $tag$...$tag$  dollar-quoted string
	TokenCast                                 // ::             PostgreSQL cast operator
)

// Sentinel errors returned by ScanParameter. Match them with errors.Is.
var (
	ErrNotParameter       = errors.New("not a parameter")
	ErrEmptyNamedParam    = errors.New("named parameter has empty name")
	ErrUnterminatedDollar = errors.New("unterminated dollar-quoted string")
)

// ScanParameter scans a single bind parameter, cast operator, or dollar-quoted
// string starting at input[pos] and returns the token plus the offset just past
// it.
//
// Recognized forms:
//
//	?           -> TokenPositional   (Literal "?")
//	$1, $42     -> TokenNumbered     (Literal "$1")
//	::          -> TokenCast         (Literal "::")
//	:name       -> TokenNamed        (Literal ":name")
//	$$x$$       -> TokenDollarString (Literal "x"; the tag is stripped)
//	$tag$x$tag$ -> TokenDollarString (Literal "x")
//
// A '$' followed by a digit is a numbered placeholder; a '$' followed by a tag
// (letters, digits, underscore) and a closing '$' opens a dollar-quoted string.
// A ':' followed by another ':' is the cast operator; otherwise it opens a named
// parameter. Any other leading byte yields ErrNotParameter.
func ScanParameter(input string, pos int) (Token, int, error) {
	if pos < 0 || pos >= len(input) {
		return Token{}, pos, ErrNotParameter
	}
	switch input[pos] {
	case '?':
		return Token{Type: TokenPositional, Literal: "?", Pos: pos}, pos + 1, nil
	case ':':
		// Maximal munch: '::' is the cast operator, tested before ':name'.
		if pos+1 < len(input) && input[pos+1] == ':' {
			return Token{Type: TokenCast, Literal: "::", Pos: pos}, pos + 2, nil
		}
		end := pos + 1
		for end < len(input) && (isLetter(input[end]) || isDigit(input[end])) {
			end++
		}
		if end == pos+1 {
			return Token{}, pos, fmt.Errorf("scan parameter at %d: %w", pos, ErrEmptyNamedParam)
		}
		return Token{Type: TokenNamed, Literal: input[pos:end], Pos: pos}, end, nil
	case '$':
		if pos+1 < len(input) && isDigit(input[pos+1]) {
			end := pos + 1
			for end < len(input) && isDigit(input[end]) {
				end++
			}
			return Token{Type: TokenNumbered, Literal: input[pos:end], Pos: pos}, end, nil
		}
		return scanDollarString(input, pos)
	default:
		return Token{}, pos, ErrNotParameter
	}
}

// scanDollarString scans a dollar-quoted string. input[pos] is '$'. The opening
// delimiter is "$tag$" (an empty tag gives "$$"); the string ends at the next
// identical delimiter. The returned Literal is the content between delimiters,
// exactly as written, with no escape processing.
func scanDollarString(input string, pos int) (Token, int, error) {
	i := pos + 1
	for i < len(input) && (isLetter(input[i]) || isDigit(input[i])) {
		i++
	}
	if i >= len(input) || input[i] != '$' {
		return Token{}, pos, fmt.Errorf("scan dollar string at %d: %w", pos, ErrUnterminatedDollar)
	}
	delim := input[pos : i+1] // "$tag$" or "$$"
	contentStart := i + 1
	rel := strings.Index(input[contentStart:], delim)
	if rel < 0 {
		return Token{}, pos, fmt.Errorf("scan dollar string at %d: %w", pos, ErrUnterminatedDollar)
	}
	content := input[contentStart : contentStart+rel]
	end := contentStart + rel + len(delim)
	return Token{Type: TokenDollarString, Literal: content, Pos: pos}, end, nil
}
```

The base lexer files are shipped unchanged; `params.go` is the only new code, and it reuses the baseline's `isLetter`/`isDigit` helpers rather than redefining them.

### The runnable demo

The demo scans each recognized form from offset zero and prints its decoded literal and the end offset, so the tag-stripping of dollar quotes and the two-byte width of the cast operator are both visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/bind-parameters"
)

func main() {
	inputs := []string{
		"?", "$1", "$42", "::int", ":name", ":user_id", "$$hello$$", "$t$a$b$t$",
	}
	for _, in := range inputs {
		tok, end, err := lexer.ScanParameter(in, 0)
		if err != nil {
			fmt.Printf("%-10s error: %v\n", in, err)
			continue
		}
		fmt.Printf("%-10s -> %-12q end=%d\n", in, tok.Literal, end)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
?          -> "?"          end=1
$1         -> "$1"         end=2
$42        -> "$42"        end=3
::int      -> "::"         end=2
:name      -> ":name"      end=5
:user_id   -> ":user_id"   end=8
$$hello$$  -> "hello"      end=9
$t$a$b$t$  -> "a$b"        end=9
```

`::int` scans just the `::` (end offset 2, leaving `int` for the caller), `$$hello$$` strips the empty tag to `hello`, and `$t$a$b$t$` keeps the inner `$b$` because only the outer `$t$` delimiter closes the string.

### Tests

The tests pin every form and every error. `TestScanParameter` table-drives the positional, numbered, cast, named, and both dollar-quote forms, plus a scan that begins partway through the input; `TestScanParameterErrors` asserts that a non-parameter byte, an out-of-range position, an empty named parameter, and the two unterminated-dollar cases each return the right sentinel under `errors.Is`. `ExampleScanParameter` pins the end-offset threading that lets a caller advance through `:name = $1`.

Create `params_test.go`:

```go
package lexer

import (
	"errors"
	"fmt"
	"testing"
)

func TestScanParameter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		pos     int
		wantTyp TokenType
		wantLit string
		wantEnd int
	}{
		{"positional", "?", 0, TokenPositional, "?", 1},
		{"numbered", "$1", 0, TokenNumbered, "$1", 2},
		{"numbered multi-digit", "$42 ", 0, TokenNumbered, "$42", 3},
		{"cast", "::int", 0, TokenCast, "::", 2},
		{"named", ":name", 0, TokenNamed, ":name", 5},
		{"named underscore", ":user_id ", 0, TokenNamed, ":user_id", 8},
		{"dollar empty tag", "$$abc$$", 0, TokenDollarString, "abc", 7},
		{"dollar named tag", "$t$a$b$t$", 0, TokenDollarString, "a$b", 9},
		{"at offset", "x = ?", 4, TokenPositional, "?", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok, end, err := ScanParameter(tc.input, tc.pos)
			if err != nil {
				t.Fatalf("ScanParameter(%q, %d) error: %v", tc.input, tc.pos, err)
			}
			if tok.Type != tc.wantTyp || tok.Literal != tc.wantLit || end != tc.wantEnd {
				t.Fatalf("ScanParameter(%q, %d) = (%d, %q, end=%d), want (%d, %q, end=%d)",
					tc.input, tc.pos, tok.Type, tok.Literal, end, tc.wantTyp, tc.wantLit, tc.wantEnd)
			}
		})
	}
}

func TestScanParameterErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		pos   int
		want  error
	}{
		{"not a parameter", "abc", 0, ErrNotParameter},
		{"out of range", "?", 5, ErrNotParameter},
		{"empty named", ": ", 0, ErrEmptyNamedParam},
		{"unterminated dollar tag", "$tag", 0, ErrUnterminatedDollar},
		{"unterminated dollar body", "$$ no close", 0, ErrUnterminatedDollar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ScanParameter(tc.input, tc.pos)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ScanParameter(%q, %d) error = %v, want errors.Is %v",
					tc.input, tc.pos, err, tc.want)
			}
		})
	}
}

func ExampleScanParameter() {
	in := ":name = $1"
	tok, next, _ := ScanParameter(in, 0)
	fmt.Printf("%q next=%d\n", tok.Literal, next)
	tok2, _, _ := ScanParameter(in, next+3) // skip " = "
	fmt.Printf("%q\n", tok2.Literal)
	// Output:
	// ":name" next=5
	// "$1"
}
```

## Review

The scanner is correct when each form is recognized by its lead byte and one byte of lookahead, and when failures are typed sentinels rather than ad-hoc strings. Confirm `::` is scanned before `:name` so the cast operator wins maximal munch, that `$1` and `$$...$$` are told apart by the byte after `$`, and that a dollar-quoted string keeps its content verbatim with the tag stripped. Confirm `ScanParameter` rejects an out-of-range `pos` with `ErrNotParameter` instead of panicking, and that every error path returns an offset the caller can ignore safely. The additive `const` block must leave the base token values untouched — that is what keeps this module compatible with the core lexer and the parser built on it.

The mistake to avoid is folding the new tokens into the base `iota` block, which renumbers every later constant and silently breaks any code that switched on the old values; the `iota + 1000` block sidesteps that entirely. The second is testing `:name` before `::`, which makes `::int` scan as an empty-named-parameter error instead of a cast — the longest form must be tried first.

## Resources

- [PostgreSQL: Dollar-Quoted String Constants](https://www.postgresql.org/docs/current/sql-syntax-lexical.html#SQL-SYNTAX-DOLLAR-QUOTING) — the `$tag$...$tag$` form, its tag matching, and why it avoids escaping.
- [PostgreSQL: Value Expressions — Positional Parameters](https://www.postgresql.org/docs/current/sql-expressions.html#SQL-EXPRESSIONS-PARAMETERS-POSITIONAL) — the `$n` numbered placeholder this scanner recognizes.
- [pkg.go.dev: errors](https://pkg.go.dev/errors) — `errors.New` and `errors.Is`, the sentinel-error pattern the scanner's failure modes use.
- [pkg.go.dev: strings.Index](https://pkg.go.dev/strings#Index) — the substring search that finds a dollar-quote's closing delimiter.

---

Back to [04-numbers-and-comments.md](04-numbers-and-comments.md) | Next: [06-token-iterator.md](06-token-iterator.md)
