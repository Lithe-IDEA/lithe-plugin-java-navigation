package codenav

// This file is a Lithe addition. Comparing against JDT LS on Nacos showed the
// only wrong jumps came from overloads with the same number of parameters
// (Error(int) vs Error(String)): the first one by arity was taken. The index
// keeps each parameter's simple type name, and a call site with several
// candidates scores them against what the arguments are known to be. Nothing
// is inferred beyond what is written: literals, casts, `new` expressions,
// declared types of locals and fields, and the expression types the resolver
// already follows. A tie keeps the first candidate, as before.

import "strings"

// call is a call site: its argument count (-1 when unknown) and the token of
// its '(' (-1 when there is none, such as a method reference).
type call struct{ ar, open int }

var noCall = call{ar: -1, open: -1}

func (q *qctx) callAt(open int) call {
	if !q.p.is(open, "(") {
		return noCall
	}
	return call{ar: q.callArity(open), open: open}
}

// paramTypes returns the simple type names of the parameters between the
// parentheses at open and end (exclusive), generics and dims stripped,
// joined by commas and interned: members with the same signature share one
// string, which keeps the index small.
func (p *parser) paramTypes(open, end int) string {
	if end-open <= 2 {
		return ""
	}
	var out []byte
	s, depth, angle := open+1, 0, 0
	for k := open + 1; k < end; k++ {
		switch {
		case k == end-1:
		case p.is(k, "(") || p.is(k, "[") || p.is(k, "{"):
			depth++
			continue
		case p.is(k, ")") || p.is(k, "]") || p.is(k, "}"):
			depth--
			continue
		case p.is(k, "<") && p.ident(k-1):
			angle++
			continue
		case p.is(k, ">") && angle > 0:
			angle--
			continue
		case p.is(k, ",") && depth == 0 && angle == 0:
		default:
			continue
		}
		name := k - 1
		for name > s && !p.ident(name) { // String args[]
			name--
		}
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, simpleName(p.typeText(s, name))...)
		s = k + 1
	}
	return p.in.s(out)
}

func simpleName(t string) string {
	return t[strings.LastIndexByte(t, '.')+1:]
}

// argType is what an argument is known to be. name is "" when unknown; t is
// set when the type was resolved; prim names a primitive; null is the
// literal null.
type argType struct {
	name string
	t    *Type
	prim string
	null bool
}

func (q *qctx) argTypes(open int) []argType {
	p := q.p
	end := p.skipBalanced(open)
	if end-open <= 2 {
		return nil
	}
	ctx := q.ctxType(open)
	var out []argType
	s := open + 1
	for k := open + 1; k < end; k++ {
		switch {
		case k == end-1:
		case p.is(k, "(") || p.is(k, "[") || p.is(k, "{"):
			k = p.skipBalanced(k) - 1
			continue
		case p.is(k, "<") && (p.is(k-1, ".") || p.ident(k-1) && isUpper(p.src[p.toks[k-1].S])):
			if e := p.skipBalanced(k); p.is(e-1, ">") && e <= end-1 {
				k = e - 1
			}
			continue
		case p.is(k, ","):
		default:
			continue
		}
		out = append(out, q.argTypeOf(s, k, ctx))
		s = k + 1
	}
	return out
}

func (q *qctx) argTypeOf(s, e int, ctx *Type) argType {
	p := q.p
	if s >= e {
		return argType{}
	}
	if p.is(s, "-") || p.is(s, "+") { // -1, +2.0
		if e-s == 2 && p.toks[s+1].Kind == tNumber {
			s++
		}
	}
	if e-s == 1 {
		switch tok := p.toks[s]; {
		case tok.Kind == tString:
			return argType{name: "String"}
		case tok.Kind == tChar:
			return argType{prim: "char"}
		case tok.Kind == tNumber:
			return argType{prim: numberType(p.txt(s))}
		}
		switch p.txt(s) {
		case "true", "false":
			return argType{prim: "boolean"}
		case "null":
			return argType{null: true}
		}
	}
	// Concatenation with a string literal at the top level is a String.
	for k := s; k < e; k++ {
		if p.is(k, "(") || p.is(k, "[") || p.is(k, "{") {
			k = p.skipBalanced(k) - 1
			continue
		}
		if p.is(k, "<") && (p.is(k-1, ".") || p.ident(k-1) && isUpper(p.src[p.toks[k-1].S])) {
			if ge := p.skipBalanced(k); p.is(ge-1, ">") && ge <= e {
				k = ge - 1
				continue
			}
		}
		if p.is(k, "+") && (p.toks[k-1].Kind == tString || k+1 < e && p.toks[k+1].Kind == tString) {
			return argType{name: "String"}
		}
		if p.is(k, "?") || p.is(k, "->") || p.is(k, "=") || p.is(k, "==") {
			return argType{} // conditional, lambda, comparison: not followed
		}
	}
	if e-s >= 3 && p.is(e-1, "class") && p.is(e-2, ".") {
		return argType{name: "Class"}
	}
	// A cast: (Foo) x, (int) y.
	if p.is(s, "(") {
		if ce := p.skipBalanced(s); ce < e && q.typeStartBack(ce-2) == s+1 && (p.ident(ce) || p.is(ce, "(") || p.toks[ce].Kind != tPunct) {
			name := simpleName(p.typeText(s+1, ce-1))
			if primitives[name] {
				return argType{prim: name}
			}
			return argType{name: name, t: q.resolveText(p.typeText(s+1, ce-1), q.file, ctx)}
		}
	}
	if t := q.typeOfExpr(e-1, ctx); t != nil {
		return argType{name: t.Name, t: t}
	}
	// Names whose declared type is written but not in the index (JDK or
	// library types without sources).
	if p.is(s, "new") && p.ident(s+1) {
		k := s + 1
		for p.is(k+1, ".") && p.ident(k+2) {
			k += 2
		}
		if name := p.txt(k); !primitives[name] && (p.is(k+1, "(") || p.is(k+1, "<")) {
			return argType{name: name}
		}
		return argType{}
	}
	if e-s == 1 && p.ident(s) {
		name := p.txt(s)
		if l, ok := q.findLocal(name, s); ok && l.typeS >= 0 {
			if text := p.typeText(l.typeS, l.typ); text != "var" {
				if tn := simpleName(text); primitives[tn] {
					return argType{prim: tn}
				} else {
					return argType{name: tn}
				}
			}
			return argType{}
		}
		if m := q.fieldInScope(name, ctx); m != nil && m.Type != "" {
			if tn := simpleName(m.Type); primitives[tn] {
				return argType{prim: tn}
			} else {
				return argType{name: tn}
			}
		}
	}
	return argType{}
}

func numberType(lit string) string {
	l := strings.ToLower(strings.ReplaceAll(lit, "_", ""))
	switch {
	case strings.HasSuffix(l, "l"):
		return "long"
	case strings.HasPrefix(l, "0x") || strings.HasPrefix(l, "0b"):
		return "int"
	case strings.HasSuffix(l, "f"):
		return "float"
	case strings.HasSuffix(l, "d") || strings.ContainsAny(l, ".e"):
		return "double"
	}
	return "int"
}

var boxes = map[string]string{"boolean": "Boolean", "byte": "Byte", "char": "Character", "short": "Short",
	"int": "Integer", "long": "Long", "float": "Float", "double": "Double"}

var widens = map[string]string{"byte": "short int long float double", "short": "int long float double",
	"char": "int long float double", "int": "long float double", "long": "float double", "float": "double"}

// finalTypes are JDK types nothing else extends, with what they can be
// passed as besides themselves.
var finalTypes = map[string]string{
	"String":    "Object CharSequence Comparable Serializable Constable ConstantDesc",
	"Class":     "Object Serializable Type GenericDeclaration AnnotatedElement",
	"Boolean":   "Object Comparable Serializable Constable",
	"Character": "Object Comparable Serializable Constable",
	"Byte":      "Object Number Comparable Serializable Constable",
	"Short":     "Object Number Comparable Serializable Constable",
	"Integer":   "Object Number Comparable Serializable Constable ConstantDesc",
	"Long":      "Object Number Comparable Serializable Constable ConstantDesc",
	"Float":     "Object Number Comparable Serializable Constable ConstantDesc",
	"Double":    "Object Number Comparable Serializable Constable ConstantDesc",
}

func hasWord(list, w string) bool {
	for _, f := range strings.Fields(list) {
		if f == w {
			return true
		}
	}
	return false
}

// typeVar reports a parameter type that is most likely a type variable (T, E).
func typeVar(p string) bool {
	return len(p) == 1 && isUpper(p[0])
}

// fit scores passing a to a parameter of type param: 3 exact, 2 by
// widening, boxing or subtyping, 1 as Object or another JDK supertype, 0
// unknown, negative when it cannot be passed.
func (q *qctx) fit(a argType, param string) int {
	switch {
	case typeVar(param):
		return 0
	case a.null:
		if primitives[param] {
			return -2
		}
		return 0
	case a.prim != "":
		switch {
		case param == a.prim:
			return 3
		case hasWord(widens[a.prim], param), param == boxes[a.prim]:
			return 2
		case primitives[param]:
			return -2
		case param == "Object" || param == "Serializable" || param == "Comparable" ||
			param == "Number" && a.prim != "boolean" && a.prim != "char":
			return 1
		}
		return -2
	case a.name == "":
		return 0
	case a.name == param:
		return 3
	}
	if primitives[param] {
		for prim, box := range boxes {
			if box == a.name && (prim == param || hasWord(widens[prim], param)) {
				return 2
			}
		}
		return -2
	}
	if supers, ok := finalTypes[a.name]; ok {
		if hasWord(supers, param) {
			return 1
		}
		return -2
	}
	if _, ok := finalTypes[param]; ok {
		return -2 // nothing but the type itself is a String or an Integer
	}
	if a.t != nil {
		for _, h := range q.hierarchy(a.t)[1:] {
			if h.Name == param {
				return 2
			}
		}
		if param == "Object" {
			return 1
		}
		return -1 // the hierarchy may leave the index through a library type
	}
	if param == "Object" {
		return 1
	}
	return 0
}

// bestOverload picks the candidate that fits the arguments at c best. As in
// Java, a candidate every argument can be passed to without varargs
// expansion wins over one that needs it; within a group the highest score
// wins and the first one wins a tie.
func (q *qctx) bestOverload(ms []*Member, c call) *Member {
	if len(ms) == 1 || c.open < 0 {
		return ms[0]
	}
	args := q.argTypes(c.open)
	rank := func(m *Member) (group, score int) {
		ok := true
		params := strings.Split(m.Params, ",")
		for k, a := range args {
			if m.Params == "" {
				break
			}
			pk := min(k, len(params)-1) // varargs elements
			f := q.fit(a, params[pk])
			ok = ok && f >= 0
			score += f
		}
		switch {
		case ok && !m.Varargs:
			return 2, score
		case ok:
			return 1, score
		}
		return 0, score
	}
	best := ms[0]
	bg, bs := rank(best)
	for _, m := range ms[1:] {
		if g, s := rank(m); g > bg || g == bg && s > bs {
			best, bg, bs = m, g, s
		}
	}
	return best
}
