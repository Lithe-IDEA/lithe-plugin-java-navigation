package codenav

import (
	"sort"
	"strings"
)

// NavTarget describes one resolved target shown in gutter marker tooltips and
// menus without exposing filesystem paths.
type NavTarget struct {
	Name     string
	Location Location
}

// NavMarker describes one bidirectional inheritance or implementation gutter
// icon for a declaration in a Java file. Line and Column are 0-based (byte
// column, matching Location).
type NavMarker struct {
	Line      int
	Column    int
	Direction string // "down" or "up"
	Relation  string // "interface" or "inheritance"
	Targets   []NavTarget
}

// Implementations returns all subtypes (for types) or overriding/implementing
// methods (for non-private, non-static, non-final methods) reachable from the
// declaration resolved at byte offset off of src in workspace file path.
func (ix *Index) Implementations(path string, src []byte, off int) []Location {
	return ix.implementations(path, false, src, off)
}

// LibraryImplementations is Implementations for a read-only JDK/dependency file.
func (ix *Index) LibraryImplementations(path string, src []byte, off int) []Location {
	return ix.implementations(path, true, src, off)
}

func (ix *Index) implementations(path string, lib bool, src []byte, off int) []Location {
	ps := parseJava(src, path, newInterner(), true)
	ps.f.Lib = lib
	if !lib {
		ix.Put(ps.f)
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	p := &parser{src: src, toks: ps.toks, f: ps.f, in: newInterner()}
	q := newQuery(ix, p, ps)
	declType, declMember, ok := ix.resolveDeclarationAt(q, ps, off)
	if !ok {
		return nil
	}
	rev := ix.reverseSubtypes(q)
	if declMember == nil {
		if declType.Kind == kAnnotation || declType.Final {
			return nil
		}
		subs := collectSubtypes(rev, declType)
		out := make([]Location, 0, len(subs))
		for _, st := range subs {
			out = append(out, typeLocation(st, st.Name))
		}
		sortLocations(out)
		return out
	}
	if declMember.Kind != kMethod || declMember.Private || declMember.Static || declMember.Final {
		return nil
	}
	subs := collectSubtypes(rev, declMember.Owner)
	var out []Location
	for _, st := range subs {
		for _, sm := range st.Members {
			if sm.Kind == kMethod && !sm.Private && !sm.Static && methodsOverrideMatch(declMember, sm) {
				out = append(out, memberLocation(sm))
			}
		}
	}
	sortLocations(out)
	return out
}

// SuperDeclarations returns direct supertype declarations (for types) or
// overridden/implemented ancestor method declarations (for methods) at byte
// offset off of src in workspace file path.
func (ix *Index) SuperDeclarations(path string, src []byte, off int) []Location {
	return ix.superDeclarations(path, false, src, off)
}

// LibrarySuperDeclarations is SuperDeclarations for a read-only library file.
func (ix *Index) LibrarySuperDeclarations(path string, src []byte, off int) []Location {
	return ix.superDeclarations(path, true, src, off)
}

func (ix *Index) superDeclarations(path string, lib bool, src []byte, off int) []Location {
	ps := parseJava(src, path, newInterner(), true)
	ps.f.Lib = lib
	if !lib {
		ix.Put(ps.f)
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	p := &parser{src: src, toks: ps.toks, f: ps.f, in: newInterner()}
	q := newQuery(ix, p, ps)
	declType, declMember, ok := ix.resolveDeclarationAt(q, ps, off)
	if !ok {
		return nil
	}
	if declMember == nil {
		var out []Location
		seen := map[*Type]bool{}
		for _, st := range q.superTypes(declType) {
			if st == nil || st == declType || seen[st] {
				continue
			}
			seen[st] = true
			out = append(out, typeLocation(st, st.Name))
		}
		sortLocations(out)
		return out
	}
	if declMember.Kind != kMethod || declMember.Private || declMember.Static {
		return nil
	}
	supers := collectSuperMethods(q, declMember, true)
	out := make([]Location, 0, len(supers))
	for _, sm := range supers {
		out = append(out, memberLocation(sm))
	}
	sortLocations(out)
	return out
}

// NavigationMarkers returns all verified bidirectional inheritance and
// implementation gutter markers for the declarations in workspace file path.
func (ix *Index) NavigationMarkers(path string, src []byte) []NavMarker {
	return ix.navigationMarkers(path, false, src)
}

// LibraryNavigationMarkers returns navigation markers for a read-only library file.
func (ix *Index) LibraryNavigationMarkers(path string, src []byte) []NavMarker {
	return ix.navigationMarkers(path, true, src)
}

func (ix *Index) navigationMarkers(path string, lib bool, src []byte) []NavMarker {
	ps := parseJava(src, path, newInterner(), false)
	ps.f.Lib = lib
	if !lib {
		ix.Put(ps.f)
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	p := &parser{src: src, toks: ps.toks, f: ps.f, in: newInterner()}
	q := newQuery(ix, p, ps)
	rev := ix.reverseSubtypes(q)

	var markers []NavMarker
	var walk func(t *Type)
	walk = func(t *Type) {
		// Type downward marker (interface implementations or class subclasses).
		if t.Kind == kInterface || (t.Kind == kClass && !t.Final) {
			if subs := collectSubtypes(rev, t); len(subs) > 0 {
				rel := "inheritance"
				if t.Kind == kInterface {
					rel = "interface"
				}
				targets := make([]NavTarget, 0, len(subs))
				for _, st := range subs {
					targets = append(targets, NavTarget{Name: st.Name, Location: typeLocation(st, st.Name)})
				}
				sortNavTargets(targets)
				markers = append(markers, NavMarker{
					Line: int(t.Line), Column: int(t.Col),
					Direction: "down", Relation: rel, Targets: targets,
				})
			}
		}

		// Type upward marker (extends / implements).
		superTypes, superRel := resolveTypeSupersWithRelation(q, t)
		if len(superTypes) > 0 {
			targets := make([]NavTarget, 0, len(superTypes))
			for _, st := range superTypes {
				targets = append(targets, NavTarget{Name: st.Name, Location: typeLocation(st, st.Name)})
			}
			sortNavTargets(targets)
			markers = append(markers, NavMarker{
				Line: int(t.Line), Column: int(t.Col),
				Direction: "up", Relation: superRel, Targets: targets,
			})
		}

		// Member markers (methods).
		for _, m := range t.Members {
			if m.Kind != kMethod || m.Private || m.Static {
				continue
			}
			if !m.Final && (t.Kind == kInterface || (t.Kind == kClass && !t.Final)) {
				subs := collectSubtypes(rev, t)
				var targets []NavTarget
				for _, st := range subs {
					for _, sm := range st.Members {
						if sm.Kind == kMethod && !sm.Private && !sm.Static && methodsOverrideMatch(m, sm) {
							targets = append(targets, NavTarget{
								Name:     st.Name,
								Location: memberLocation(sm),
							})
						}
					}
				}
				if len(targets) > 0 {
					sortNavTargets(targets)
					rel := "inheritance"
					if t.Kind == kInterface || m.Abstract {
						rel = "interface"
					}
					markers = append(markers, NavMarker{
						Line: int(m.Line), Column: int(m.Col),
						Direction: "down", Relation: rel, Targets: targets,
					})
				}
			}

			if superMethods := collectSuperMethods(q, m, false); len(superMethods) > 0 {
				rel := "inheritance"
				targets := make([]NavTarget, 0, len(superMethods))
				for _, sm := range superMethods {
					if sm.Owner.Kind == kInterface || sm.Abstract {
						rel = "interface"
					}
					targets = append(targets, NavTarget{
						Name:     sm.Owner.Name + "." + sm.Name + "()",
						Location: memberLocation(sm),
					})
				}
				sortNavTargets(targets)
				markers = append(markers, NavMarker{
					Line: int(m.Line), Column: int(m.Col),
					Direction: "up", Relation: rel, Targets: targets,
				})
			}
		}

		for _, nested := range t.Nested {
			walk(nested)
		}
	}

	file := ix.files[path]
	if file == nil {
		file = ps.f
	}
	for _, t := range file.Types {
		walk(t)
	}
	sort.Slice(markers, func(i, j int) bool {
		if markers[i].Line != markers[j].Line {
			return markers[i].Line < markers[j].Line
		}
		return markers[i].Direction < markers[j].Direction
	})
	return markers
}

func (ix *Index) resolveDeclarationAt(q *qctx, ps *parsed, off int) (*Type, *Member, bool) {
	p := q.p
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
		return nil, nil, false
	}
	hits := q.resolveAt(ti)
	if len(hits) == 0 {
		return nil, nil, false
	}
	h := hits[0]
	f := ix.fileOf(Location{Path: h.File, Library: h.Lib})
	if f == nil {
		return nil, nil, false
	}
	var walk func(t *Type) (*Type, *Member)
	walk = func(t *Type) (*Type, *Member) {
		if t.Line == h.Line && t.Col == h.Col {
			return t, nil
		}
		for _, m := range t.Members {
			if m.Line == h.Line && m.Col == h.Col {
				return t, m
			}
		}
		for _, c := range t.Nested {
			if dt, dm := walk(c); dt != nil {
				return dt, dm
			}
		}
		return nil, nil
	}
	for _, t := range f.Types {
		if dt, dm := walk(t); dt != nil {
			return dt, dm, true
		}
	}
	return nil, nil, false
}

func (ix *Index) reverseSubtypes(q *qctx) map[*Type][]*Type {
	rev := make(map[*Type][]*Type)
	var walk func(t *Type)
	walk = func(t *Type) {
		for _, sup := range q.superTypes(t) {
			if sup != nil && sup != t {
				rev[sup] = append(rev[sup], t)
			}
		}
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, f := range ix.files {
		for _, t := range f.Types {
			walk(t)
		}
	}
	return rev
}

func collectSubtypes(rev map[*Type][]*Type, root *Type) []*Type {
	visited := map[*Type]bool{root: true}
	queue := []*Type{root}
	var subs []*Type
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range rev[cur] {
			if visited[child] {
				continue
			}
			visited[child] = true
			subs = append(subs, child)
			queue = append(queue, child)
		}
	}
	return subs
}

func resolveTypeSupersWithRelation(q *qctx, t *Type) ([]*Type, string) {
	var out []*Type
	seen := map[*Type]bool{t: true}
	rel := "inheritance"
	for _, s := range t.Extends {
		if st := q.resolveText(s, t.File, t.Outer); st != nil && !seen[st] {
			seen[st] = true
			out = append(out, st)
			if st.Kind == kInterface {
				rel = "interface"
			}
		}
	}
	for _, s := range t.Implements {
		if st := q.resolveText(s, t.File, t.Outer); st != nil && !seen[st] {
			seen[st] = true
			out = append(out, st)
			rel = "interface"
		}
	}
	return out, rel
}

// collectSuperMethods walks t's ancestor chain (with cycle guard) and collects
// matching overridden/implemented methods. includeObject covers java.lang.Object
// overrides when requested or for well-known Object methods.
func collectSuperMethods(q *qctx, m *Member, includeObject bool) []*Member {
	visited := map[*Type]bool{m.Owner: true}
	queue := append([]*Type(nil), q.superTypes(m.Owner)...)
	var ancestors []*Type
	for len(queue) > 0 && len(visited) < 64 {
		cur := queue[0]
		queue = queue[1:]
		if cur == nil || visited[cur] {
			continue
		}
		visited[cur] = true
		ancestors = append(ancestors, cur)
		queue = append(queue, q.superTypes(cur)...)
	}
	if includeObject || isObjectOverridable(m.Name) {
		if obj := q.impliedType("java.lang.Object"); obj != nil && !visited[obj] {
			ancestors = append(ancestors, obj)
		}
	}
	var found []*Member
	seenMember := map[*Member]bool{}
	for _, anc := range ancestors {
		for _, sm := range anc.Members {
			if sm.Kind == kMethod && !sm.Private && !sm.Static && methodsOverrideMatch(sm, m) && !seenMember[sm] {
				seenMember[sm] = true
				found = append(found, sm)
			}
		}
	}
	return found
}

func isObjectOverridable(name string) bool {
	switch name {
	case "toString", "equals", "hashCode", "clone", "finalize":
		return true
	default:
		return false
	}
}

func methodsOverrideMatch(base, sub *Member) bool {
	if base.Kind != kMethod || sub.Kind != kMethod || base.Name != sub.Name {
		return false
	}
	if base.Arity != sub.Arity {
		return false
	}
	if base.Params == sub.Params || base.Params == "" || sub.Params == "" {
		return true
	}
	bParts := strings.Split(base.Params, ",")
	sParts := strings.Split(sub.Params, ",")
	if len(bParts) != len(sParts) {
		return false
	}
	for i := range bParts {
		if bParts[i] == sParts[i] {
			continue
		}
		// Allow a type variable to stand for a substituted type, also as an array component.
		bType, sType := bParts[i], sParts[i]
		bElem, sElem := strings.TrimRight(bType, "[]"), strings.TrimRight(sType, "[]")
		bDims, sDims := len(bType)-len(bElem), len(sType)-len(sElem)
		if bDims <= sDims && isGenericTypeParam(bElem) || sDims <= bDims && isGenericTypeParam(sElem) {
			continue
		}
		return false
	}
	return true
}

func isGenericTypeParam(name string) bool {
	return len(name) == 1 && name[0] >= 'A' && name[0] <= 'Z'
}

func typeLocation(t *Type, name string) Location {
	return Location{
		Path: t.File.Path, Library: t.File.Lib,
		Line: int(t.Line), Column: int(t.Col), Name: name,
	}
}

func memberLocation(m *Member) Location {
	return Location{
		Path: m.Owner.File.Path, Library: m.Owner.File.Lib,
		Line: int(m.Line), Column: int(m.Col), Name: m.Name,
	}
}

func sortLocations(locs []Location) {
	sort.Slice(locs, func(i, j int) bool {
		if locs[i].Path != locs[j].Path {
			return locs[i].Path < locs[j].Path
		}
		if locs[i].Line != locs[j].Line {
			return locs[i].Line < locs[j].Line
		}
		return locs[i].Column < locs[j].Column
	})
}

func sortNavTargets(targets []NavTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Location.Path != targets[j].Location.Path {
			return targets[i].Location.Path < targets[j].Location.Path
		}
		if targets[i].Location.Line != targets[j].Location.Line {
			return targets[i].Location.Line < targets[j].Location.Line
		}
		return targets[i].Location.Column < targets[j].Location.Column
	})
}
