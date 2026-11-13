package codenav

import (
	"unicode"
	"unicode/utf8"
)

// A Java tokenizer that only keeps what declaration and reference analysis
// needs: identifiers, punctuation, and opaque literals. Comments are dropped;
// string, char and text-block literals become one token so their contents
// never look like code.

type tokKind uint8

const (
	tIdent tokKind = iota
	tPunct
	tString
	tChar
	tNumber
)

type token struct {
	S, E int32 // byte offsets in src
	Line int32 // 0-based line of S
	Kind tokKind
}

var javaKeywords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{
		"abstract", "assert", "boolean", "break", "byte", "case", "catch", "char", "class", "const",
		"continue", "default", "do", "double", "else", "enum", "extends", "final", "finally", "float",
		"for", "goto", "if", "implements", "import", "instanceof", "int", "interface", "long", "native",
		"new", "package", "private", "protected", "public", "return", "short", "static", "strictfp",
		"super", "switch", "synchronized", "this", "throw", "throws", "transient", "try", "void",
		"volatile", "while", "true", "false", "null",
	} {
		m[w] = true
	}
	return m
}()

func lex(src []byte) []token {
	return lexInto(make([]token, 0, len(src)/5), src)
}

// lexInto appends the tokens of src to toks, which a caller parsing many
// files reuses: the tokens of a file are scratch once its declarations are
// read, and allocating them anew was half of what indexing allocated.
func lexInto(toks []token, src []byte) []token {
	line := int32(0)
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i < n && !(src[i] == '*' && i+1 < n && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case c == '"' && i+2 < n && src[i+1] == '"' && src[i+2] == '"':
			s, l := i, line
			i += 3
			for i < n && !(src[i] == '"' && i+2 < n && src[i+1] == '"' && src[i+2] == '"') {
				if src[i] == '\\' {
					i++
				} else if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 3
			toks = append(toks, token{int32(s), int32(min(i, n)), l, tString})
		case c == '"' || c == '\'':
			s := i
			i++
			for i < n && src[i] != c && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
			k := tString
			if c == '\'' {
				k = tChar
			}
			toks = append(toks, token{int32(s), int32(min(i, n)), line, k})
		case c >= '0' && c <= '9' || c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			s := i
			for i < n && (isIdentByte(src[i]) || src[i] == '.' ||
				(src[i] == '+' || src[i] == '-') && (src[i-1] == 'e' || src[i-1] == 'E' || src[i-1] == 'p' || src[i-1] == 'P')) {
				i++
			}
			toks = append(toks, token{int32(s), int32(i), line, tNumber})
		case isIdentByte(c) || c >= 0x80:
			s := i
			for i < n {
				if src[i] < 0x80 {
					if !isIdentByte(src[i]) {
						break
					}
					i++
					continue
				}
				r, sz := utf8.DecodeRune(src[i:])
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					break
				}
				i += sz
			}
			if i == s { // a non-letter non-ASCII byte: skip it
				_, sz := utf8.DecodeRune(src[i:])
				i += sz
				continue
			}
			toks = append(toks, token{int32(s), int32(i), line, tIdent})
		default:
			w := 1
			if i+1 < n {
				if c == '-' && src[i+1] == '>' || c == ':' && src[i+1] == ':' {
					w = 2
				}
			}
			if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
				w = 3
			}
			toks = append(toks, token{int32(i), int32(i + w), line, tPunct})
			i += w
		}
	}
	return toks
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
