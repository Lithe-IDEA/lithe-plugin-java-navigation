package lsp

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestPositionsInUTF16AndUTF8(t *testing.T) {
	// "名" is 3 bytes and 1 UTF-16 unit; "😀" is 4 bytes and 2 units.
	text := []byte("class A {}\r\nString 名 = \"😀\"; int x;\nlast")
	xByte := len("String 名 = \"😀\"; int ")
	cases := []struct {
		enc  encoding
		char int
	}{
		{utf16Units, len([]rune("String 名 = \"")) + 2 + len("\"; int ")},
		{utf8Bytes, xByte},
	}
	for _, c := range cases {
		off := offsetOf(text, Position{Line: 1, Character: c.char}, c.enc)
		if got := string(text[off : off+1]); got != "x" {
			t.Errorf("%s: offset %d is %q, want x", c.enc, off, got)
		}
		if p := positionOf(text, 1, xByte, c.enc); p != (Position{Line: 1, Character: c.char}) {
			t.Errorf("%s: positionOf = %+v, want character %d", c.enc, p, c.char)
		}
	}
	// A character past the line end clamps to it, before the "\r\n".
	if off := offsetOf(text, Position{Line: 0, Character: 99}, utf16Units); off != len("class A {}") {
		t.Errorf("clamped offset = %d", off)
	}
	// A line past the end clamps to the end of the text.
	if off := offsetOf(text, Position{Line: 9, Character: 0}, utf16Units); off != len(text) {
		t.Errorf("past-end offset = %d, want %d", off, len(text))
	}
	// Inside a surrogate pair the position stays before the character.
	emoji := len([]rune("String 名 = \""))
	if off := offsetOf(text, Position{Line: 1, Character: emoji + 1}, utf16Units); string(text[off:off+4]) != "😀" {
		t.Errorf("offset inside a surrogate pair = %q", text[off:])
	}
}

func TestNameRange(t *testing.T) {
	text := []byte("@Data\nclass 名前 { int x; }")
	if r := nameRange(text, 0, 0, utf16Units); r.Start.Character != 0 || r.End.Character != 5 {
		t.Errorf("annotation range = %+v, want 0..5", r)
	}
	if r := nameRange(text, 1, len("class "), utf16Units); r.Start.Character != 6 || r.End.Character != 8 {
		t.Errorf("non-ASCII identifier range = %+v, want 6..8", r)
	}
	if r := nameRange(text, 1, len("class "), utf8Bytes); r.Start.Character != 6 || r.End.Character != 12 {
		t.Errorf("UTF-8 range = %+v, want 6..12", r)
	}
	if r := nameRange(text, 5, 0, utf16Units); r.Start != r.End {
		t.Errorf("range past the end = %+v, want empty", r)
	}
}

func TestIncrementalChanges(t *testing.T) {
	d := &document{text: []byte("class A {\n  int x;\n}\n")}
	changes := []contentChange{
		{Range: &Range{Start: Position{1, 6}, End: Position{1, 7}}, Text: "count"},
		{Range: &Range{Start: Position{0, 6}, End: Position{0, 7}}, Text: "名"},
		{Range: &Range{Start: Position{2, 1}, End: Position{2, 1}}, Text: " // end"},
	}
	if err := d.apply(changes, utf16Units); err != nil {
		t.Fatal(err)
	}
	if want := "class 名 {\n  int count;\n} // end\n"; string(d.text) != want {
		t.Errorf("text = %q, want %q", d.text, want)
	}
	if err := d.apply([]contentChange{{Text: "replaced"}}, utf16Units); err != nil || string(d.text) != "replaced" {
		t.Errorf("full change = %q, %v", d.text, err)
	}
	bad := []contentChange{{Range: &Range{Start: Position{0, 5}, End: Position{0, 1}}, Text: ""}}
	if err := d.apply(bad, utf16Units); err == nil {
		t.Error("a reversed range was accepted")
	}
}

func TestURIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "my src", "名前", "A#1.java")
	u := uriOf(p)
	back, err := pathOf(u)
	if err != nil || back != filepath.Clean(p) {
		t.Errorf("pathOf(%s) = %q, %v; want %q", u, back, err, p)
	}
	if runtime.GOOS == "windows" {
		if got, err := pathOf("file:///c%3A/x/A.java"); err != nil || got != `c:\x\A.java` {
			t.Errorf("encoded drive letter = %q, %v", got, err)
		}
	}
	if _, err := pathOf("jdt://contents/x"); err == nil {
		t.Error("a non-file URI was accepted")
	}
}
