package lsp

import (
	"bytes"
	"unicode/utf16"
	"unicode/utf8"
)

// encoding is how a position's character counts within a line: UTF-16 code
// units (the protocol's default) or bytes, when the client offers UTF-8.
type encoding int

const (
	utf16Units encoding = iota
	utf8Bytes
)

func (e encoding) String() string {
	if e == utf8Bytes {
		return "utf-8"
	}
	return "utf-16"
}

// Position is a zero-based line and character.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// lineBounds returns the byte span of the 0-based line of text, without its
// line terminator; ok is false past the last line.
func lineBounds(text []byte, line int) (start, end int, ok bool) {
	if line < 0 {
		return 0, 0, false
	}
	start = 0
	for i := 0; i < line; i++ {
		j := bytes.IndexByte(text[start:], '\n')
		if j < 0 {
			return 0, 0, false
		}
		start += j + 1
	}
	end = start
	if j := bytes.IndexByte(text[start:], '\n'); j >= 0 {
		end = start + j
	} else {
		end = len(text)
	}
	if end > start && text[end-1] == '\r' {
		end--
	}
	return start, end, true
}

// offsetOf converts p to a byte offset of text. A character past the end of
// its line clamps to the line end, and a line past the end of text to the end
// of text, as the protocol asks.
func offsetOf(text []byte, p Position, enc encoding) int {
	start, end, ok := lineBounds(text, p.Line)
	if !ok {
		return len(text)
	}
	return start + byteColumn(text[start:end], p.Character, enc)
}

// byteColumn converts a character count within line to a byte count.
func byteColumn(line []byte, char int, enc encoding) int {
	if char <= 0 {
		return 0
	}
	if enc == utf8Bytes {
		return min(char, len(line))
	}
	units, i := 0, 0
	for i < len(line) && units < char {
		r, size := utf8.DecodeRune(line[i:])
		n := 1
		if r >= 0x10000 && r != utf8.RuneError {
			n = len(utf16.Encode([]rune{r}))
		}
		if units+n > char {
			break // inside a surrogate pair: stay before the character
		}
		units += n
		i += size
	}
	return i
}

// charColumn converts a byte count within line to a character count.
func charColumn(line []byte, col int, enc encoding) int {
	col = max(0, min(col, len(line)))
	if enc == utf8Bytes {
		return col
	}
	units := 0
	for i := 0; i < col; {
		r, size := utf8.DecodeRune(line[i:])
		if r >= 0x10000 && r != utf8.RuneError {
			units += 2
		} else {
			units++
		}
		i += size
	}
	return units
}

// positionOf converts a 0-based line and byte column of text to a position.
func positionOf(text []byte, line, col int, enc encoding) Position {
	start, end, ok := lineBounds(text, line)
	if !ok {
		return Position{Line: line, Character: 0}
	}
	return Position{Line: line, Character: charColumn(text[start:end], col, enc)}
}

// nameRange is the range of the name at the 0-based line and byte column of
// text: the identifier there (an annotation's '@' included), or an empty
// range when there is none.
func nameRange(text []byte, line, col int, enc encoding) Range {
	start, end, ok := lineBounds(text, line)
	if !ok {
		p := Position{Line: line}
		return Range{Start: p, End: p}
	}
	l := text[start:end]
	col = max(0, min(col, len(l)))
	e := col
	if e < len(l) && l[e] == '@' {
		e++
	}
	for e < len(l) && identByte(l[e]) {
		e++
	}
	return Range{
		Start: Position{Line: line, Character: charColumn(l, col, enc)},
		End:   Position{Line: line, Character: charColumn(l, e, enc)},
	}
}

// identByte matches codenav's lexer: ASCII letters, digits, '_', '$' and any
// byte of a multi-byte UTF-8 sequence.
func identByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 0x80
}
