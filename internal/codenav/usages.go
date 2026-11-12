package codenav

import "bytes"

// Usages are found the way an editor without a language server can afford:
// a file is a candidate only if one of the names occurs in it as a whole
// word; a candidate is parsed like the file under the cursor and every
// occurrence is resolved with the same rules as go to declaration. An
// occurrence is a usage when it navigates to the declaration, so a usage
// list never contradicts what Command+Click on one of its rows would do.
//
// Package and import declarations are not usages: an import only makes a
// name visible, and listing it would turn every single real usage into two.

// UsageNames returns the identifiers whose occurrences can navigate to decl,
// declared as name: name itself and, for a field with Lombok accessors, the
// getter and setter names that navigate to the field.
func (ix *Index) UsageNames(decl Location, name string) []string {
	names := []string{name}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	m := ix.memberAt(decl)
	if m == nil || m.Kind != kField {
		return names
	}
	lb := m.Owner.Lombok | m.Lombok
	if lb&lombokGetter != 0 {
		names = append(names, "get"+capital(name), "is"+capital(name))
	}
	if lb&lombokSetter != 0 {
		names = append(names, "set"+capital(name))
	}
	return names
}

// Usages lists the places in src, the current content of path, that navigate
// to decl: occurrences of names (see UsageNames) other than the declaration
// itself and outside package and import declarations, in source order. It
// does not change the index, so it can run on several goroutines at once and
// on content that differs from what is indexed for path.
func (ix *Index) Usages(path string, src []byte, decl Location, names []string) []Location {
	return ix.usages(path, false, src, decl, names)
}

// LibraryUsages is Usages for src, the content of the library source at path:
// the usages of a local variable declared there.
func (ix *Index) LibraryUsages(path string, src []byte, decl Location, names []string) []Location {
	return ix.usages(path, true, src, decl, names)
}

func (ix *Index) usages(path string, lib bool, src []byte, decl Location, names []string) []Location {
	if !containsName(src, names) {
		return nil
	}
	ps := parseJava(src, path, newInterner(), true)
	ps.f.Lib = lib
	p := &parser{src: src, toks: ps.toks, f: ps.f, in: newInterner()}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	q := newQuery(ix, p, ps)
	target := ix.typeAt(decl)
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []Location
	for i := 0; i < len(ps.toks); i++ {
		if p.is(i, "package") || p.is(i, "import") {
			for i < len(ps.toks) && !p.is(i, ";") {
				i++
			}
			continue
		}
		if !p.ident(i) || !want[p.txt(i)] {
			continue
		}
		tok := ps.toks[i]
		col := p.col(tok)
		if path == decl.Path && lib == decl.Library && int(tok.Line) == decl.Line && int(col) == decl.Column {
			continue
		}
		hits := q.resolveAt(i)
		if len(hits) == 0 || !refersTo(hits[0], decl, target) {
			continue
		}
		out = append(out, Location{Path: path, Library: lib, Line: int(tok.Line), Column: int(col), Name: p.txt(i), rule: hits[0].Why})
	}
	return out
}

// refersTo reports whether h, what an occurrence navigates to, is decl. For a
// type, a constructor call counts too: "new User()" navigates to the
// constructor (or the Lombok annotation generating it), but it uses the type.
func refersTo(h hit, decl Location, t *Type) bool {
	if h.File == decl.Path && h.Lib == decl.Library && int(h.Line) == decl.Line && int(h.Col) == decl.Column {
		return true
	}
	if t == nil || h.Why != "ctor" || h.File != t.File.Path || h.Lib != t.File.Lib {
		return false
	}
	at := pos{h.Line, h.Col}
	if at == t.NoArgsAt || at == t.AllArgsAt || at == t.ReqArgsAt {
		return true
	}
	for _, m := range t.Members {
		if m.Kind == kCtor && m.Line == h.Line && m.Col == h.Col {
			return true
		}
	}
	return false
}

// typeAt returns the indexed or library type declared at loc, or nil. The
// caller holds the read lock.
func (ix *Index) typeAt(loc Location) *Type {
	f := ix.fileOf(loc)
	if f == nil {
		return nil
	}
	var found *Type
	var walk func(t *Type)
	walk = func(t *Type) {
		if int(t.Line) == loc.Line && int(t.Col) == loc.Column {
			found = t
		}
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range f.Types {
		walk(t)
	}
	return found
}

// memberAt returns the indexed or library member declared at loc, or nil.
// The caller holds the read lock.
func (ix *Index) memberAt(loc Location) *Member {
	f := ix.fileOf(loc)
	if f == nil {
		return nil
	}
	var found *Member
	var walk func(t *Type)
	walk = func(t *Type) {
		for _, m := range t.Members {
			if int(m.Line) == loc.Line && int(m.Col) == loc.Column {
				found = m
			}
		}
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range f.Types {
		walk(t)
	}
	return found
}

// containsName reports whether one of names occurs in src as a whole word,
// the cheap test that keeps most files from being parsed at all. A match
// inside a comment or string still passes; parsing sorts those out.
func containsName(src []byte, names []string) bool {
	for _, n := range names {
		if n == "" {
			continue
		}
		b := []byte(n)
		for from := 0; ; {
			k := bytes.Index(src[from:], b)
			if k < 0 {
				break
			}
			s, e := from+k, from+k+len(b)
			if (s == 0 || !nameByte(src[s-1])) && (e == len(src) || !nameByte(src[e])) {
				return true
			}
			from = s + 1
		}
	}
	return false
}

// nameByte reports a byte that can continue a Java name; every byte of a
// multi-byte rune counts, so non-ASCII names are matched whole.
func nameByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}
