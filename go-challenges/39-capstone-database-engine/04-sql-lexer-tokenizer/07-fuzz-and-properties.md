# Exercise 7: Property and Fuzz Tests

The lexer's two load-bearing invariants are stated in prose throughout this lesson: it never panics, and it never fails to terminate. This exercise turns them into executable checks — a fuzz target over arbitrary input, a deterministic random property test over a SQL-flavored alphabet, and two round-trip properties: one where concatenating token literals must reconstruct the source with whitespace removed, and one for quoted identifiers where that raw round-trip cannot hold once `""` escaping is in play.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             the baseline token model
keywords.go          the baseline keyword map
lexer.go             the baseline lexer
cmd/
  demo/
    main.go          tokenize hostile inputs and report each terminates cleanly
fuzz_test.go         never-panics fuzz + random property, two round-trip properties
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `cmd/demo/main.go`, `fuzz_test.go`.
- Implement: the demo's `label` helper and the four checks — `FuzzLexerNeverPanics`, `TestLexerNeverPanicsRandom`, `TestRoundTripProperty`, `TestRoundTripQuotedIdentProperty` — plus `ExampleTokenize_errorTerminates`.
- Test: `go test -race ./...` runs the seed corpus and the property tests; `go test -fuzz=FuzzLexerNeverPanics` runs real fuzzing.
- Verify: `go test -run 'TestLexer|TestRoundTrip|Example' -race ./...`

### Why fuzzing and properties, not just examples

Table-driven tests check the inputs the author thought of; the lexer's contract is about the inputs nobody thought of. "Never panics on any input" and "always terminates" are universal claims, and the honest way to test a universal claim is to throw unplanned input at it. Go's native fuzzing does exactly that: `FuzzLexerNeverPanics` seeds a corpus of nasty cases — an unterminated string, an unbalanced nested comment, raw punctuation, a truncated exponent — and `go test -fuzz` then mutates them to explore the input space, failing the moment `Tokenize` panics or returns a stream whose last token is neither `TokenEOF` nor `TokenError`. Under a plain `go test` the same target runs just the seed corpus, so the contract is checked on every CI run even without a fuzzing budget.

`TestLexerNeverPanicsRandom` complements the fuzzer with a deterministic, race-checked sweep: a seeded PCG generator (`math/rand/v2`) draws thousands of short strings from a SQL-flavored alphabet — quotes, comment leads, operators, digits, newlines — and asserts each tokenizes without panicking and terminates. Seeding the generator makes the test reproducible, which a raw fuzzer is not; the two are complementary, not redundant. The recover-and-fail wrapper around each call turns a panic into a test failure with the offending input attached, which is what makes a failure actionable.

The round-trip properties pin maximal munch from the other direction. `TestRoundTripProperty` builds random sources from space-separated keywords, identifiers, integers, and single-character operators, and asserts that concatenating the token literals reproduces the source with spaces removed — proving that space-separated tokens never merge and no two-character operator forms across a gap. The quoted-identifier property is the instructive counter-case: once `"d""e"` decodes to `d"e`, the literal is shorter than its source span and the surrounding quotes are gone, so the raw concatenation property *cannot* hold. The right invariant there is that each `TokenQIdent` literal equals the unquoted, un-doubled value — a reminder that a round-trip property must be stated over the decoded value, not the source bytes, wherever escaping is involved.

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

The whole baseline ships here so the properties run against the real lexer; this exercise adds only test code and the demo.

### The runnable demo

The demo is the contract in miniature: it tokenizes four inputs — one well-formed and three hostile — and reports for each how many tokens came back and whether the last one is `EOF` or `Error`. None of them panic, and every stream terminates, which is exactly what the fuzz and property tests assert at scale.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/fuzz-and-properties"
)

func label(t lexer.TokenType) string {
	switch t {
	case lexer.TokenEOF:
		return "EOF"
	case lexer.TokenError:
		return "Error"
	default:
		return "other"
	}
}

func main() {
	inputs := []string{"SELECT 1", "'oops", "/* /* */", "@#$%"}
	for _, in := range inputs {
		toks := lexer.Tokenize(in)
		last := toks[len(toks)-1]
		fmt.Printf("%-12q -> %d tokens, last=%s\n", in, len(toks), label(last.Type))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"SELECT 1"   -> 3 tokens, last=EOF
"'oops"      -> 1 tokens, last=Error
"/* /* */"   -> 1 tokens, last=Error
"@#$%"       -> 1 tokens, last=Error
```

The well-formed `SELECT 1` ends in `EOF`; each malformed input ends in a single `Error` token rather than a panic, which is the totality property the tests enforce over millions of inputs.

### Tests

The tests encode the two invariants and the two round-trip properties. `FuzzLexerNeverPanics` carries the seed corpus and the never-panic/always-terminate assertion; `TestLexerNeverPanicsRandom` runs the deterministic random sweep; `TestRoundTripProperty` pins literal concatenation over the un-escaped subset; and `TestRoundTripQuotedIdentProperty` pins the decoded-value invariant for quoted identifiers, documenting why the raw round-trip does not apply there. `ExampleTokenize_errorTerminates` pins that a malformed input's last token is a `TokenError`.

Create `fuzz_test.go`:

```go
package lexer

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// FuzzLexerNeverPanics asserts the core contract: on ANY input the lexer
// neither panics nor fails to terminate. `go test` runs the seed corpus;
// `go test -fuzz=FuzzLexerNeverPanics` runs real fuzzing.
func FuzzLexerNeverPanics(f *testing.F) {
	seeds := []string{
		"", "SELECT", "'unterminated", "/* /* */", "@#$%",
		"1.2e", `"x""y"`, "SELECT * FROM t WHERE a >= 1 AND b <> 2;",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("lexer panicked on %q: %v", in, r)
			}
		}()
		toks := Tokenize(in)
		if len(toks) == 0 {
			t.Fatalf("Tokenize(%q) returned no tokens", in)
		}
		last := toks[len(toks)-1].Type
		if last != TokenEOF && last != TokenError {
			t.Fatalf("Tokenize(%q) did not terminate with EOF/Error, got %d", in, last)
		}
	})
}

// TestLexerNeverPanicsRandom complements the fuzz corpus with deterministic,
// race-checked coverage over random byte strings drawn from a SQL-flavored
// alphabet (quotes, comment leads, operators, digits).
func TestLexerNeverPanicsRandom(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []byte("SELECTfrom_*/-+ '\"$:?()=<>.,;0123456789ab\n\t")
	for i := 0; i < 3000; i++ {
		n := rng.IntN(40)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[rng.IntN(len(alphabet))]
		}
		s := string(b)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("lexer panicked on %q: %v", s, r)
				}
			}()
			toks := Tokenize(s)
			last := toks[len(toks)-1].Type
			if last != TokenEOF && last != TokenError {
				t.Fatalf("Tokenize(%q) did not terminate, got %d", s, last)
			}
		}()
	}
}

// TestRoundTripProperty asserts that for a defined subset of SQL — keywords in
// canonical case, simple identifiers, integers, and single-character operators,
// each separated by one space — concatenating the token literals reconstructs
// the source with whitespace removed. This pins maximal munch: space-separated
// tokens never merge, and no two-character operator forms across a gap.
func TestRoundTripProperty(t *testing.T) {
	t.Parallel()

	pool := []string{
		"SELECT", "FROM", "WHERE", "AND",
		"id", "name", "x",
		"1", "42", "100",
		"+", "-", "*", "=", "<", ">", "(", ")", ",", ";", ".",
	}
	rng := rand.New(rand.NewPCG(42, 99))
	for i := 0; i < 1500; i++ {
		n := 1 + rng.IntN(12)
		parts := make([]string, n)
		for j := range parts {
			parts[j] = pool[rng.IntN(len(pool))]
		}
		src := strings.Join(parts, " ")
		var sb strings.Builder
		for _, tok := range Tokenize(src) {
			if tok.Type == TokenEOF {
				break
			}
			if tok.Type == TokenError {
				t.Fatalf("unexpected error token for %q: %q", src, tok.Literal)
			}
			sb.WriteString(tok.Literal)
		}
		want := strings.ReplaceAll(src, " ", "")
		if sb.String() != want {
			t.Fatalf("round trip mismatch:\n src: %q\n got: %q\nwant: %q", src, sb.String(), want)
		}
	}
}

// TestRoundTripQuotedIdentProperty checks the quoted-identifier invariant the
// raw round-trip cannot: once "" escaping is in play, concatenating literals no
// longer reproduces the source (the doubled quote collapses to one and the
// surrounding quotes are gone), so the property is instead that each TokenQIdent
// literal equals the unquoted, un-doubled value.
func TestRoundTripQuotedIdentProperty(t *testing.T) {
	t.Parallel()

	pool := []struct{ src, unquoted string }{
		{`"a"`, "a"},
		{`"b c"`, "b c"},
		{`"d""e"`, `d"e`},
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 1000; i++ {
		n := 1 + rng.IntN(6)
		idx := make([]int, n)
		srcParts := make([]string, n)
		for j := range idx {
			idx[j] = rng.IntN(len(pool))
			srcParts[j] = pool[idx[j]].src
		}
		src := strings.Join(srcParts, " ")
		toks := Tokenize(src)
		var q int
		for _, tok := range toks {
			if tok.Type == TokenEOF {
				break
			}
			if tok.Type != TokenQIdent {
				t.Fatalf("src %q: got non-quoted token %v", src, tok)
			}
			if want := pool[idx[q]].unquoted; tok.Literal != want {
				t.Fatalf("src %q token %d: literal %q, want %q", src, q, tok.Literal, want)
			}
			q++
		}
		if q != n {
			t.Fatalf("src %q: scanned %d quoted idents, want %d", src, q, n)
		}
	}
}

func ExampleTokenize_errorTerminates() {
	toks := Tokenize("'oops")
	last := toks[len(toks)-1]
	fmt.Println(last.Type == TokenError)
	// Output:
	// true
}
```

## Review

The suite is sound when it tests universals as universals. Confirm `FuzzLexerNeverPanics` both runs its seed corpus under plain `go test` and is available to `go test -fuzz` for real mutation, and that it fails on either a panic or a non-terminating stream. Confirm the random sweep is seeded (and therefore reproducible) and race-clean, and that the round-trip property holds for the space-separated subset while the quoted-identifier property is stated over the decoded value rather than the raw bytes. A failure in any of these should print the offending input, because an un-actionable property failure is barely better than no test.

The mistake to avoid is asserting the raw concatenation round-trip over inputs that contain escapes: `"d""e"` decodes to `d"e`, so the literal cannot reproduce its source span, and a test that expects it to will fail on correct code. State the property over the decoded value wherever doubling or escaping is in play. The second is using an unseeded generator in the "deterministic" test, which makes a failure impossible to reproduce; `rand.NewPCG` with fixed seeds keeps the sweep repeatable.

## Resources

- [Go: Fuzzing](https://go.dev/security/fuzz/) — how native fuzzing seeds a corpus, mutates inputs, and reports a minimized failing case.
- [pkg.go.dev: testing — Fuzzing](https://pkg.go.dev/testing#hdr-Fuzzing) — the `*testing.F` API, `f.Add`, and `f.Fuzz` used by the target.
- [pkg.go.dev: math/rand/v2](https://pkg.go.dev/math/rand/v2) — the PCG generator that makes the random property test deterministic.
- [PostgreSQL: Lexical Structure](https://www.postgresql.org/docs/current/sql-syntax-lexical.html) — the authoritative reference the round-trip and quoted-identifier properties are checked against.

---

Back to [06-token-iterator.md](06-token-iterator.md) | Next: [SQL Parser](../05-sql-parser/00-concepts.md)
