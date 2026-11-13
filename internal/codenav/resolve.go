package codenav

import (
	"strings"
)

// Query-time resolution. Only the file under the cursor is lexed and parsed
// (with its method bodies); everything else comes from the index. The rules,
// in the order Java scoping applies them: the declaration itself, lambda
// parameters, annotations and their attributes, enum constants in a case
// label, qualified access (a.b, a.b(), Type::m) through the static type of
// the qualifier, constructor calls, unqualified calls and names through the
// enclosing types and their supertypes, locals and parameters by block scope,
// type parameters, and finally types through the imports. Lombok-generated
// members (accessors, builders, loggers, constructors) navigate to the field
// or annotation that generates them.
//
// A name the working tree does not declare is looked up in the index's
// Library (the JDK), where java.lang is imported implicitly and every type
// inherits java.lang.Object's members; a name found in neither (a library
// jar) yields no location rather than a guess.

// Location is a declaration: a repository-relative path (or, with Library
// set, a library path) and the 0-based line and byte column of the declared
// name (or of the Lombok annotation that generates it).
type Location struct {
	Path         string
	Library      bool
	Line, Column int
	// Name is the identifier that was looked up (or, for a usage, the
	// identifier found there). For a Lombok accessor it is the accessor's
	// name, not the field's.
	Name string
	// Self reports that the identifier looked up is the declaration itself:
	// the place where an editor offers its usages instead.
	Self bool
	// Local reports a declaration only its own file can refer to: a local
	// variable, a parameter or a type parameter.
	Local bool
	rule  string // the rule that produced it, for tests
}

// hit is the internal form of a Location.
type hit struct {
	File      string
	Lib       bool // File is a library path
	Line, Col int32
	Why       string
}

type qctx struct {
	ix       *Index
	p        *parser // helpers over the current file's tokens
	ps       *parsed
	file     *File
	depth    int
	supers   map[*Type][]*Type // resolved supertypes, for this query only
	builders map[*Type]*Type   // owner -> synthetic Lombok builder type
	implied  map[string]*Type  // java.lang supertypes looked up so far, nil if absent
}

type local struct {
	tok        int
	typeS, typ int // type token range [typeS, typ); typeS<0 means untyped
}

// Resolve answers "go to declaration" for the identifier at byte offset off
// of src, the current content of the indexed file path. It parses src and
// replaces path's entry with it, so the index never answers from an older
// version of the file the user is looking at. It reports false when the
// offset is not on an identifier or the declaration is not in the index or
// its library.
func (ix *Index) Resolve(path string, src []byte, off int) (Location, bool) {
	return ix.resolve(path, false, src, off)
}

// ResolveLibrary is Resolve for src, the content of the library source at
// path. Library sources do not change, so the index is left alone.
func (ix *Index) ResolveLibrary(path string, src []byte, off int) (Location, bool) {
	return ix.resolve(path, true, src, off)
}

func (ix *Index) resolve(path string, lib bool, src []byte, off int) (Location, bool) {
	ps := parseJava(src, path, newInterner(), true)
	ps.f.Lib = lib
	if !lib {
		ix.Put(ps.f)
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p := &parser{src: src, toks: ps.toks, f: ps.f, in: newInterner()}
	q := newQuery(ix, p, ps)
	ti := -1
	for i, t := range ps.toks {
		if int(t.S) <= off && off < int(t.E) {
			ti = i
			break
		}
		if int(t.S) > off {
			break
		}
	}
	if ti < 0 || !p.ident(ti) || javaKeywords[p.txt(ti)] {
		return Location{}, false
	}
	locs := q.resolveAt(ti)
	if len(locs) == 0 {
		return Location{}, false
	}
	l := locs[0]
	tok := ps.toks[ti]
	return Location{
		Path: l.File, Library: l.Lib, Line: int(l.Line), Column: int(l.Col), Name: p.txt(ti),
		Self:  l.File == path && l.Lib == lib && l.Line == tok.Line && l.Col == p.col(tok),
		Local: l.Why == "decl-local" || l.Why == "local" || l.Why == "type-param",
		rule:  l.Why,
	}, true
}

func newQuery(ix *Index, p *parser, ps *parsed) *qctx {
	return &qctx{ix: ix, p: p, ps: ps, file: ps.f, supers: map[*Type][]*Type{}, builders: map[*Type]*Type{},
		implied: map[string]*Type{}}
}

func (q *qctx) loc(t *Type, why string) []hit {
	if t == nil {
		return nil
	}
	return []hit{{t.File.Path, t.File.Lib, t.Line, t.Col, why}}
}

func (q *qctx) mloc(m *Member, why string) []hit {
	if m == nil {
		return nil
	}
	return []hit{{m.Owner.File.Path, m.Owner.File.Lib, m.Line, m.Col, why}}
}

// here is a hit in the current file.
func (q *qctx) here(line, col int32, why string) []hit {
	return []hit{{q.file.Path, q.file.Lib, line, col, why}}
}

func (q *qctx) ctxType(ti int) *Type {
	var best *Type
	bestLen := 1 << 30
	for _, r := range q.ps.ranges {
		if r.start <= ti && ti < r.end && r.end-r.start < bestLen {
			best, bestLen = r.t, r.end-r.start
		}
	}
	return best
}

func (q *qctx) bodyAt(ti int) *body {
	var best *body
	bestLen := 1 << 30
	for i := range q.ps.bodies {
		b := &q.ps.bodies[i]
		if b.start <= ti && ti < b.end && b.end-b.start < bestLen {
			best, bestLen = b, b.end-b.start
		}
	}
	return best
}

func (q *qctx) resolveAt(ti int) []hit {
	p := q.p
	name := p.txt(ti)
	tok := p.toks[ti]
	col := p.col(tok)
	ctx := q.ctxType(ti)

	// Declaration site: the identifier names a type or member declared here.
	var decl []hit
	var walk func(t *Type)
	walk = func(t *Type) {
		if t.Line == tok.Line && t.Col == col {
			decl = q.loc(t, "decl")
		}
		for _, m := range t.Members {
			if m.Line == tok.Line && m.Col == col {
				decl = q.mloc(m, "decl")
			}
		}
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range q.file.Types {
		walk(t)
	}
	if decl != nil {
		return decl
	}

	isCall := p.is(ti+1, "(")
	self := q.here(tok.Line, col, "decl-local")
	if q.isLambdaParam(ti) {
		return self
	}
	// A method declared inside a body (anonymous or local class).
	if isCall && q.typeStartBack(ti-1) >= 0 {
		if e := p.skipBalanced(ti + 1); p.is(e, "{") || p.is(e, "throws") {
			return self
		}
	}
	if p.is(ti-1, "@") {
		return q.loc(q.resolveSimple(name, q.file, ctx), "annotation")
	}
	if p.is(ti+1, "=") && !p.is(ti+2, "=") {
		if at, isAnn := q.annotationOf(ti); isAnn {
			// An attribute of a library annotation (@Schema(description=...))
			// lives outside the project: never fall back to a same-named field.
			if at == nil {
				return nil
			}
			return q.mloc(q.findMethod(at, name, call{ar: 0, open: -1}), "annotation-attr")
		}
	}
	if p.is(ti-1, "case") || p.is(ti-1, ",") && q.inCaseLabel(ti) {
		if st := q.switchType(ti, ctx); st != nil {
			if m := q.findField(st, name); m != nil {
				return q.mloc(m, "case-const")
			}
		}
	}
	dot := q.qualifierDot(ti)
	switch {
	case dot >= 0:
		ref := p.is(dot, "::")
		ot := q.typeOfExpr(dot-1, ctx)
		if ot == nil {
			// A package-qualified type name: com.foo.Bar
			if t := q.qualifiedType(ti); t != nil {
				return q.loc(t, "fqn")
			}
			return nil
		}
		if q.annotationName(dot) { // @Outer.Inner (Lithe addition)
			return q.loc(q.findNested(ot, name), "annotation")
		}
		if isCall && q.afterNew(ti) {
			if c := q.pickCtor(ot, q.callAt(ti+1)); c != nil && ot.Name == name {
				return q.mloc(c, "ctor")
			}
			if t := q.findNested(ot, name); t != nil {
				if c := q.pickCtor(t, q.callAt(ti+1)); c != nil {
					return q.mloc(c, "ctor")
				}
				return q.loc(t, "new-type")
			}
		}
		if isCall || ref {
			c := noCall
			if isCall {
				c = q.callAt(ti + 1)
			}
			if m := q.findMethod(ot, name, c); m != nil {
				return q.mloc(m, "member-call")
			}
			return nil
		}
		if m := q.findField(ot, name); m != nil {
			return q.mloc(m, "member-field")
		}
		if t := q.findNested(ot, name); t != nil {
			return q.loc(t, "nested")
		}
		return nil
	case p.is(ti-1, "new"):
		t := q.resolveSimple(name, q.file, ctx)
		if t == nil {
			return nil
		}
		if isCall || p.is(ti+1, "<") {
			open := ti + 1
			if p.is(open, "<") {
				open = p.skipBalanced(open)
			}
			if c := q.pickCtor(t, q.callAt(open)); c != nil {
				return q.mloc(c, "ctor")
			}
		}
		return q.loc(t, "new-type")
	case isCall:
		if m := q.methodInScope(name, q.callAt(ti+1), ctx); m != nil {
			return q.mloc(m, "call")
		}
		return nil
	}
	if l, ok := q.findLocal(name, ti); ok {
		return q.here(p.toks[l.tok].Line, p.col(p.toks[l.tok]), "local")
	}
	if m := q.fieldInScope(name, ctx); m != nil {
		return q.mloc(m, "field")
	}
	if tp, ok := q.typeParam(name, ti); ok {
		return q.here(p.toks[tp].Line, p.col(p.toks[tp]), "type-param")
	}
	if t := q.resolveSimple(name, q.file, ctx); t != nil {
		return q.loc(t, "type")
	}
	return nil
}

func (q *qctx) typeParam(name string, ti int) (int, bool) {
	best, bl := -1, 1<<30
	for _, tp := range q.ps.tparams {
		if tp.s <= ti && ti < tp.e && tp.e-tp.s < bl && q.p.txt(tp.tok) == name {
			best, bl = tp.tok, tp.e-tp.s
		}
	}
	return best, best >= 0
}

// afterNew reports whether the qualified name ending at ti follows 'new'.
func (q *qctx) afterNew(ti int) bool {
	p := q.p
	s := ti
	for p.is(s-1, ".") && p.ident(s-2) {
		s -= 2
	}
	return p.is(s-1, "new")
}

// isLambdaParam reports whether the identifier declares a lambda parameter.
func (q *qctx) isLambdaParam(ti int) bool {
	p := q.p
	if p.is(ti-1, ".") {
		return false
	}
	if p.is(ti+1, "->") {
		return true
	}
	if !p.is(ti+1, ",") && !p.is(ti+1, ")") {
		return false
	}
	// (a, b) -> / (Foo a, Bar b) ->: walk to the closing paren.
	for k := ti + 1; k < len(p.toks); k++ {
		switch {
		case p.is(k, ")"):
			return p.is(k+1, "->")
		case p.ident(k) || p.is(k, ",") || p.is(k, ".") || p.is(k, "[") || p.is(k, "]"):
		case p.is(k, "<"):
			k = p.skipBalanced(k) - 1
		default:
			return false
		}
	}
	return false
}

// enclosingOpen returns the index of the unclosed '(' containing ti.
func (q *qctx) enclosingOpen(ti int, open, close string) int {
	p := q.p
	d := 0
	for k := ti - 1; k >= 0; k-- {
		switch {
		case p.is(k, close):
			d++
		case p.is(k, open):
			if d == 0 {
				return k
			}
			d--
		case open == "(" && (p.is(k, ";") || p.is(k, "{") || p.is(k, "}")):
			return -1
		}
	}
	return -1
}

// annotationOf returns the annotation type whose argument list contains ti.
func (q *qctx) annotationOf(ti int) (*Type, bool) {
	p := q.p
	o := q.enclosingOpen(ti, "(", ")")
	if o < 1 || !p.ident(o-1) {
		return nil, false
	}
	s := o - 1
	for p.is(s-1, ".") && p.ident(s-2) {
		s -= 2
	}
	if !p.is(s-1, "@") {
		return nil, false
	}
	var parts []string
	for k := s; k < o; k += 2 {
		parts = append(parts, p.txt(k))
	}
	return q.resolveText(strings.Join(parts, "."), q.file, q.ctxType(ti)), true
}

func (q *qctx) inCaseLabel(ti int) bool {
	p := q.p
	for k := ti - 1; k >= 0 && k > ti-64; k-- {
		if p.is(k, "case") {
			return true
		}
		if !p.ident(k) && !p.is(k, ",") && !p.is(k, ".") {
			return false
		}
	}
	return false
}

// switchType infers the selector type of the switch whose body contains ti.
func (q *qctx) switchType(ti int, ctx *Type) *Type {
	p := q.p
	o := ti
	for {
		o = q.enclosingOpen(o, "{", "}")
		if o < 1 {
			return nil
		}
		if p.is(o-1, ")") {
			po := q.matchBack(o - 1)
			if po > 0 && p.is(po-1, "switch") {
				return q.typeOfExpr(o-2, ctx)
			}
		}
	}
}

// ---- types and hierarchy ----

var primitives = map[string]bool{"boolean": true, "byte": true, "char": true, "short": true,
	"int": true, "long": true, "float": true, "double": true, "void": true, "var": true}

// lookupFQN finds a type by its fully qualified name in the working tree,
// then in the library: a project's own java.lang.String (a JDK checkout)
// wins over the JDK's.
func (q *qctx) lookupFQN(fqn string, from *File) *Type {
	ts := q.ix.byFQN[fqn]
	if len(ts) == 0 {
		if q.ix.lib != nil {
			return q.ix.lib.lookup(fqn)
		}
		return nil
	}
	if len(ts) == 1 || from == nil {
		return ts[0]
	}
	// Duplicate FQNs across modules: prefer the one sharing the longest path prefix.
	best, bl := ts[0], -1
	for _, t := range ts {
		l := commonPrefix(t.File.Path, from.Path)
		if l > bl {
			best, bl = t, l
		}
	}
	return best
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func (q *qctx) resolveSimple(name string, f *File, ctx *Type) *Type {
	if primitives[name] {
		return nil
	}
	for t := ctx; t != nil; t = t.Outer {
		if t.Name == name {
			return t
		}
		if n := q.findNested(t, name); n != nil {
			return n
		}
	}
	if fqn, ok := f.Imports[name]; ok {
		if t := q.lookupFQN(fqn, f); t != nil {
			return t
		}
		return nil // imported from a library
	}
	// A member type named by a single static import ("import static
	// a.B.Nested;") shadows the package like a single-type import.
	if t := q.staticMemberType(name, f, false); t != nil {
		return t
	}
	if f.Pkg != "" {
		if t := q.lookupFQN(f.Pkg+"."+name, f); t != nil {
			return t
		}
	} else if t := q.lookupFQN(name, f); t != nil {
		return t
	}
	for _, w := range f.Wild {
		if t := q.lookupFQN(w+"."+name, f); t != nil {
			return t
		}
	}
	// Every compilation unit imports java.lang on demand.
	if t := q.lookupFQN("java.lang."+name, f); t != nil {
		return t
	}
	// Static on-demand imports ("import static a.B.*;") rank with the other
	// on-demand imports.
	return q.staticMemberType(name, f, true)
}

// staticMemberType finds a member type called name of a type named by f's
// static imports: the single imports of that name, or (wild) the on-demand
// ones.
func (q *qctx) staticMemberType(name string, f *File, wild bool) *Type {
	for _, s := range f.Static {
		i := strings.LastIndexByte(s, '.')
		if i < 0 || wild != (s[i+1:] == "*") || !wild && s[i+1:] != name {
			continue
		}
		if owner := q.lookupFQN(s[:i], f); owner != nil {
			if t := q.findNested(owner, name); t != nil {
				return t
			}
		}
	}
	return nil
}

// resolveText resolves a type as written ("Foo", "Outer.Inner", "a.b.Foo").
func (q *qctx) resolveText(text string, f *File, ctx *Type) *Type {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, ".")
	t := q.resolveSimple(parts[0], f, ctx)
	rest := parts[1:]
	if t == nil {
		for k := len(parts); k >= 2; k-- {
			if t = q.lookupFQN(strings.Join(parts[:k], "."), f); t != nil {
				rest = parts[k:]
				break
			}
		}
	}
	for _, p := range rest {
		if t == nil {
			return nil
		}
		t = q.findNested(t, p)
	}
	return t
}

func (q *qctx) superTypes(t *Type) []*Type {
	if s, ok := q.supers[t]; ok {
		return s
	}
	q.supers[t] = nil // cycle guard
	var out []*Type
	for _, s := range t.Supers {
		if st := q.resolveText(s, t.File, t.Outer); st != nil && st != t {
			out = append(out, st)
		}
	}
	q.supers[t] = out
	return out
}

// hierarchy returns t and all its supertypes, breadth first: those written
// in the sources, then the ones Java implies, which only a library provides:
// java.lang.Enum for an enum, java.lang.Record for a record and
// java.lang.Object for every type (an interface has Object's public methods
// too).
func (q *qctx) hierarchy(t *Type) []*Type {
	seen := map[*Type]bool{t: true}
	out := []*Type{t}
	for i := 0; i < len(out) && len(out) < 64; i++ {
		for _, s := range q.superTypes(out[i]) {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	implied := []string{"java.lang.Object"}
	switch t.Kind {
	case kEnum:
		implied = []string{"java.lang.Enum", "java.lang.Object"}
	case kRecord:
		implied = []string{"java.lang.Record", "java.lang.Object"}
	}
	for _, fqn := range implied {
		if s := q.impliedType(fqn); s != nil && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// impliedType looks up a java.lang supertype once per query.
func (q *qctx) impliedType(fqn string) *Type {
	t, ok := q.implied[fqn]
	if !ok {
		t = q.lookupFQN(fqn, nil)
		q.implied[fqn] = t
	}
	return t
}

func (q *qctx) findNested(t *Type, name string) *Type {
	for _, h := range q.hierarchy(t) {
		for _, n := range h.Nested {
			if n.Name == name {
				return n
			}
		}
	}
	return nil
}

func arityOK(m *Member, ar int) bool {
	if ar < 0 {
		return true
	}
	if m.Varargs {
		return ar >= int(m.Arity)-1
	}
	return int(m.Arity) == ar
}

// synth makes a Lombok-generated member that navigates to annotation at.
func synth(owner *Type, name string, at pos, ret *Type) *Member {
	return &Member{Name: name, Kind: kMethod, Owner: owner, Line: at.Line, Col: at.Col, Ret: ret}
}

func (q *qctx) builderOf(t *Type) *Type {
	if b, ok := q.builders[t]; ok {
		return b
	}
	b := &Type{Name: t.Name + "Builder", FQN: t.FQN + "." + t.Name + "Builder", Kind: kClass, File: t.File,
		Line: t.BuilderAt.Line, Col: t.BuilderAt.Col, Outer: t, BuilderAt: noPos, LogAt: noPos}
	q.builders[t] = b
	return b
}

func (q *qctx) findMethod(t *Type, name string, c call) *Member {
	ar := c.ar
	// Members of a synthetic @Builder type: one setter per field, and build().
	if t.Outer != nil && q.builders[t.Outer] == t {
		owner := t.Outer
		if name == "build" {
			return synth(owner, name, owner.BuilderAt, owner)
		}
		if q.findField(owner, name) != nil {
			return synth(owner, name, owner.BuilderAt, t)
		}
		return nil
	}
	var first *Member
	var fits []*Member
	h := q.hierarchy(t)
	for _, ht := range h {
		for _, m := range ht.Members {
			if m.Kind == kMethod && m.Name == name {
				if arityOK(m, ar) {
					fits = append(fits, m)
				}
				if first == nil {
					first = m
				}
			}
		}
	}
	if len(fits) > 0 {
		return q.bestOverload(fits, c)
	}
	if first != nil {
		return first
	}
	if name == "builder" {
		for _, ht := range h {
			if ht.BuilderAt.Line >= 0 {
				return synth(ht, name, ht.BuilderAt, q.builderOf(t))
			}
		}
	}
	// Record accessors and Lombok-generated accessors point at the field.
	for _, ht := range h {
		for _, m := range ht.Members {
			if m.Kind != kField {
				continue
			}
			if ht.Kind == kRecord && m.Name == name && ar <= 0 {
				return m
			}
			lb := ht.Lombok | m.Lombok
			if lb == 0 {
				continue
			}
			if lb&lombokGetter != 0 && ar <= 0 && (name == "get"+capital(m.Name) || name == "is"+capital(m.Name)) ||
				lb&lombokSetter != 0 && ar == 1 && name == "set"+capital(m.Name) {
				return m
			}
		}
	}
	return nil
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (q *qctx) findField(t *Type, name string) *Member {
	h := q.hierarchy(t)
	for _, ht := range h {
		for _, m := range ht.Members {
			if (m.Kind == kField || m.Kind == kEnumConst) && m.Name == name {
				return m
			}
		}
	}
	if name == "log" {
		for _, ht := range h {
			if ht.LogAt.Line >= 0 {
				return &Member{Name: name, Kind: kField, Type: ht.LogType, Owner: ht, Line: ht.LogAt.Line, Col: ht.LogAt.Col}
			}
		}
	}
	return nil
}

func (q *qctx) pickCtor(t *Type, c call) *Member {
	ar := c.ar
	var first *Member
	var fits []*Member
	for _, m := range t.Members {
		if m.Kind == kCtor {
			if arityOK(m, ar) {
				fits = append(fits, m)
			}
			if first == nil {
				first = m
			}
		}
	}
	if len(fits) > 0 {
		return q.bestOverload(fits, c)
	}
	if first != nil {
		return first
	}
	// Lombok-generated constructors navigate to their annotation.
	order := []pos{t.ReqArgsAt, t.AllArgsAt, t.NoArgsAt}
	if ar == 0 {
		order = []pos{t.NoArgsAt, t.ReqArgsAt, t.AllArgsAt}
	} else if ar > 0 {
		order = []pos{t.AllArgsAt, t.ReqArgsAt, t.NoArgsAt}
	}
	for _, at := range order {
		if at.Line >= 0 {
			return &Member{Name: t.Name, Kind: kCtor, Owner: t, Line: at.Line, Col: at.Col}
		}
	}
	return nil
}

func (q *qctx) staticOwners(name string) []*Type {
	var out []*Type
	for _, s := range q.file.Static {
		i := strings.LastIndexByte(s, '.')
		if i < 0 || (s[i+1:] != name && s[i+1:] != "*") {
			continue
		}
		if t := q.resolveText(s[:i], q.file, nil); t != nil {
			out = append(out, t)
		}
	}
	return out
}

func (q *qctx) methodInScope(name string, c call, ctx *Type) *Member {
	for t := ctx; t != nil; t = t.Outer {
		if m := q.findMethod(t, name, c); m != nil {
			return m
		}
	}
	for _, t := range q.staticOwners(name) {
		if m := q.findMethod(t, name, c); m != nil {
			return m
		}
	}
	return nil
}

func (q *qctx) fieldInScope(name string, ctx *Type) *Member {
	for t := ctx; t != nil; t = t.Outer {
		if m := q.findField(t, name); m != nil {
			return m
		}
	}
	for _, t := range q.staticOwners(name) {
		if m := q.findField(t, name); m != nil {
			return m
		}
	}
	return nil
}

func (q *qctx) memberType(m *Member) *Type {
	if m.Kind == kEnumConst {
		return m.Owner
	}
	return q.resolveText(m.Type, m.Owner.File, m.Owner)
}

// ---- expressions ----

func (q *qctx) callArity(open int) int {
	p := q.p
	if !p.is(open, "(") {
		return -1
	}
	a, _ := p.arity(open, p.skipBalanced(open))
	return int(a)
}

func (q *qctx) matchBack(close int) int {
	p := q.p
	c := p.src[p.toks[close].S]
	var open byte = '('
	if c == ']' {
		open = '['
	}
	depth := 0
	for i := close; i >= 0; i-- {
		t := p.toks[i]
		if t.Kind != tPunct {
			continue
		}
		switch p.src[t.S] {
		case c:
			depth++
		case open:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (q *qctx) qualifiedType(ti int) *Type {
	p := q.p
	s := ti
	for p.is(s-1, ".") && p.ident(s-2) {
		s -= 2
	}
	var parts []string
	for i := s; i <= ti; i += 2 {
		parts = append(parts, p.txt(i))
	}
	return q.lookupFQN(strings.Join(parts, "."), q.file)
}

// typeOfExpr infers the static type of the expression ending at token end.
func (q *qctx) typeOfExpr(end int, ctx *Type) *Type {
	p := q.p
	if end < 0 || q.depth > 16 {
		return nil
	}
	q.depth++
	defer func() { q.depth-- }()
	switch {
	case p.toks[end].Kind == tString: // "text".trim(), and text blocks
		return q.impliedType("java.lang.String")
	case p.ident(end):
		s := p.txt(end)
		if p.is(end-1, ".") {
			switch s {
			case "this": // Outer.this
				return q.resolveSimple(p.txt(end-2), q.file, ctx)
			case "class": // Foo.class.getName()
				return q.impliedType("java.lang.Class")
			case "super": // Iface.super.m(): the interface; Outer.super.m(): Outer's superclass
				t := q.resolveSimple(p.txt(end-2), q.file, ctx)
				if t == nil || t.Kind == kInterface {
					return t
				}
				if ss := q.superTypes(t); len(ss) > 0 {
					return ss[0]
				}
				return nil
			}
			ot := q.typeOfExpr(end-2, ctx)
			if ot == nil {
				return q.qualifiedType(end)
			}
			if m := q.findField(ot, s); m != nil {
				return q.memberType(m)
			}
			return q.findNested(ot, s)
		}
		switch s {
		case "this":
			return ctx
		case "super":
			if ctx != nil {
				if ss := q.superTypes(ctx); len(ss) > 0 {
					return ss[0]
				}
			}
			return nil
		}
		if l, ok := q.findLocal(s, end); ok {
			return q.localType(l, ctx)
		}
		if m := q.fieldInScope(s, ctx); m != nil {
			return q.memberType(m)
		}
		return q.resolveSimple(s, q.file, ctx)
	case p.is(end, ")"):
		open := q.matchBack(end)
		if open < 0 {
			return nil
		}
		before := open - 1
		if p.is(before, ">") { // new Foo<>(...)
			d := 0
			for ; before >= 0; before-- {
				if p.is(before, ">") {
					d++
				} else if p.is(before, "<") {
					d--
					if d == 0 {
						break
					}
				}
			}
			before--
		}
		if p.ident(before) {
			nm := p.txt(before)
			if q.afterNew(before) {
				s := before
				for p.is(s-1, ".") && p.ident(s-2) {
					s -= 2
				}
				var parts []string
				for k := s; k <= before; k += 2 {
					parts = append(parts, p.txt(k))
				}
				return q.resolveText(strings.Join(parts, "."), q.file, ctx)
			}
			if nm == "this" || nm == "super" || javaKeywords[nm] && nm != "this" {
				return nil
			}
			c := q.callAt(open)
			var m *Member
			if dot := q.qualifierDot(before); dot >= 0 {
				if ot := q.typeOfExpr(dot-1, ctx); ot != nil {
					m = q.findMethod(ot, nm, c)
				}
			} else {
				m = q.methodInScope(nm, c, ctx)
			}
			if m == nil {
				return nil
			}
			if m.Ret != nil {
				return m.Ret
			}
			if m.Kind == kField { // record/Lombok accessor
				// A chained setter compiles only if it returns this
				// (@Accessors(chain = true) or lombok.config).
				if strings.HasPrefix(nm, "set") && nm != m.Name {
					return m.Owner
				}
				return q.memberType(m)
			}
			return q.resolveText(m.Type, m.Owner.File, m.Owner)
		}
		// cast: ((Foo) x)
		if p.is(open+1, "(") {
			ce := p.skipBalanced(open + 1)
			return q.resolveText(p.typeText(open+2, ce-1), q.file, ctx)
		}
		return q.typeOfExpr(end-1, ctx)
	case p.is(end, "]"):
		open := q.matchBack(end)
		return q.typeOfExpr(open-1, ctx) // element type: dims are not tracked
	}
	return nil
}

func (q *qctx) localType(l local, ctx *Type) *Type {
	p := q.p
	if l.typeS < 0 {
		return nil
	}
	text := p.typeText(l.typeS, l.typ)
	if text == "var" {
		if p.is(l.tok+1, "=") {
			e := p.skipExpr(l.tok + 2)
			return q.typeOfExpr(e-1, ctx)
		}
		return nil
	}
	return q.resolveText(text, q.file, ctx)
}

// typeStartBack returns the first token of a type that ends at token e, or -1.
func (q *qctx) typeStartBack(e int) int {
	p := q.p
	for p.is(e, "]") && p.is(e-1, "[") {
		e -= 2
	}
	if p.is(e, ">") {
		d := 0
		for ; e >= 0; e-- {
			switch {
			case p.is(e, ">"):
				d++
			case p.is(e, "<"):
				d--
			case p.ident(e) || p.is(e, ".") || p.is(e, ",") || p.is(e, "?") || p.is(e, "[") || p.is(e, "]"):
			default:
				return -1
			}
			if d == 0 {
				break
			}
		}
		e--
	}
	if !p.ident(e) {
		return -1
	}
	s := p.txt(e)
	if javaKeywords[s] && !primitives[s] || s == "yield" {
		return -1
	}
	for p.is(e-1, ".") && p.ident(e-2) {
		e -= 2
	}
	// What precedes a declaration's type.
	b := e - 1
	if b < 0 {
		return e
	}
	if p.ident(b) {
		bs := p.txt(b)
		if bs == "instanceof" || bs == "var" || modifiers[bs] {
			return e
		}
		// A bare annotation: @RequestParam String name
		a := b
		for p.is(a-1, ".") && p.ident(a-2) {
			a -= 2
		}
		if p.is(a-1, "@") {
			return e
		}
		return -1
	}
	switch p.txt(b) {
	case "(", ",", ";", "{", "}", ")", ":", "->", "<", "&&", "&", "|":
		return e
	}
	return -1
}

// findLocal finds the nearest in-scope declaration of name before token at.
func (q *qctx) findLocal(name string, at int) (local, bool) {
	p := q.p
	b := q.bodyAt(at)
	if b == nil {
		var r *typeRange
		for i := range q.ps.ranges {
			x := &q.ps.ranges[i]
			if x.start <= at && at < x.end && (r == nil || x.end-x.start < r.end-r.start) {
				r = x
			}
		}
		if r == nil {
			return local{}, false
		}
		b = &body{start: r.start, end: r.end}
	}
	var decls []local
	var names []string
	var marks []int
	add := func(k, ts, te int) {
		decls = append(decls, local{k, ts, te})
		names = append(names, p.txt(k))
	}
	// A declaration of several variables (int i = 0, n = s.length();)
	// gives the names after its commas the first one's type (Lithe
	// addition). multi is that declaration while its statement lasts, at
	// paren depth multiDepth; the initializers' arguments are deeper.
	var multi *local
	depth, multiDepth := 0, 0
	for k := b.start; k <= at && k < len(p.toks); k++ {
		switch {
		case p.is(k, "{"):
			marks = append(marks, len(decls))
			multi = nil
			continue
		case p.is(k, "}"):
			if n := len(marks); n > 0 {
				decls, names = decls[:marks[n-1]], names[:marks[n-1]]
				marks = marks[:n-1]
			}
			multi = nil
			continue
		case p.is(k, "("):
			depth++
			continue
		case p.is(k, ")"):
			depth--
			if multi != nil && depth < multiDepth {
				multi = nil
			}
			continue
		case p.is(k, ";"):
			if multi != nil && depth <= multiDepth {
				multi = nil
			}
			continue
		case p.is(k, "->"):
			if p.ident(k-1) && !p.is(k-2, ".") {
				add(k-1, -1, -1)
			} else if p.is(k-1, ")") {
				o := q.matchBack(k - 1)
				for j := o + 1; j < k-1; j++ {
					if p.ident(j) && (p.is(j+1, ",") || p.is(j+1, ")")) && q.typeStartBack(j-1) < 0 {
						add(j, -1, -1)
					}
				}
			}
			continue
		}
		if !p.ident(k) || javaKeywords[p.txt(k)] {
			continue
		}
		nx := k + 1
		ok := p.is(nx, "=") && !p.is(nx+1, "=") || p.is(nx, ";") || p.is(nx, ",") || p.is(nx, ")") || p.is(nx, ":")
		if !ok && (p.is(nx, "&") || p.is(nx, "|") || p.is(nx, "?")) {
			if ts := q.typeStartBack(k - 1); ts > 0 && p.is(ts-1, "instanceof") {
				ok = true
			}
		}
		if !ok {
			continue
		}
		te := k
		if p.is(k-1, "...") { // varargs parameter (Lithe addition)
			te = k - 1
		}
		if ts := q.typeStartBack(te - 1); ts >= 0 {
			add(k, ts, te)
			if !p.is(nx, ")") && !p.is(nx, ":") {
				multi, multiDepth = &local{k, ts, te}, depth
			}
		} else if multi != nil && depth == multiDepth && p.is(k-1, ",") && !p.is(nx, ":") {
			add(k, multi.typeS, multi.typ)
		}
	}
	for i := len(decls) - 1; i >= 0; i-- {
		if names[i] == name {
			return decls[i], true
		}
	}
	return local{}, false
}
