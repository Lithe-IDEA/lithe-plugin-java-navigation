package codenav

import (
	"strings"
)

// The declaration parser. It reads one Java file and keeps what navigation
// needs: the package, the imports, every type (with its supertypes as written)
// and every member (with its declared type as written). It skips method bodies
// and initializers instead of parsing them; only the file under the cursor is
// parsed again with its body ranges kept (keep=true), so locals can be found.
// It never builds an expression tree and never fails: malformed input yields
// fewer declarations, not an error.

// declKind is what a declaration declares.
type declKind uint8

const (
	kClass declKind = iota
	kInterface
	kEnum
	kRecord
	kAnnotation
	kMethod
	kCtor
	kField
	kEnumConst
)

// File is the declarations of one source file, as ParseFile read them. The
// index owns it after Put; callers treat it as opaque.
type File struct {
	Path    string // repo-relative, slash-separated
	Pkg     string
	Imports map[string]string // simple name -> FQN (single-type imports)
	Wild    []string          // on-demand imports: package or type FQNs
	Static  []string          // static imports: "a.b.C.member" or "a.b.C.*"
	Types   []*Type
	// Lib marks a library source (the JDK): Path is then a library path,
	// not a working-tree one (see Library).
	Lib bool
}

// Type is a class, interface, enum, record or annotation type declaration.
// Line and Col locate its name (0-based line, byte column).
type Type struct {
	Name, FQN string
	Kind      declKind
	File      *File
	Line, Col int32
	Outer     *Type
	Supers    []string // as written, generics stripped
	Members   []*Member
	Nested    []*Type
	Lombok    uint8 // lombokGetter | lombokSetter
	// Positions of @Builder and @Slf4j-style annotations (-1 if absent):
	// Lombok-generated members have no source of their own, so navigation
	// lands on the annotation, as JDTLS does.
	BuilderAt, LogAt pos
	// Lombok constructor annotations: no-args, all-args, required-args (@Data/@Value too).
	NoArgsAt, AllArgsAt, ReqArgsAt pos
}

// pos is a 0-based line and byte column.
type pos struct{ Line, Col int32 }

var noPos = pos{-1, -1}

const (
	lombokGetter = 1 << iota
	lombokSetter
)

// Member is a method, constructor, field or enum constant of a Type.
type Member struct {
	Name      string
	Kind      declKind
	Type      string // declared/return type, generics and dims stripped; "" for ctors
	Arity     int16
	Varargs   bool
	Owner     *Type
	Line, Col int32
	Lombok    uint8 // field-level @Getter/@Setter
	Ret       *Type // synthetic members: the return type, already resolved
}

// parsed is the per-file parse result. The body ranges are only used for the
// file under the cursor and are dropped for the index.
type parsed struct {
	f       *File
	bodies  []body
	ranges  []typeRange
	tparams []tparam
	toks    []token
}

type body struct {
	owner      *Type
	member     *Member // nil for initializer blocks
	start, end int     // token range: from the parameter list (or '{') to after '}'
}

type typeRange struct {
	t          *Type
	start, end int // token indexes of the body braces
}

type parser struct {
	src     []byte
	toks    []token
	f       *File
	in      *interner
	bodies  []body
	ranges  []typeRange
	tparams []tparam
	keep    bool
}

// interner shares the strings of one parse (type names repeat a lot). It is
// per file on purpose: an index-wide table would keep the names of removed
// files alive for the whole session.
type interner struct {
	m map[string]string
}

func newInterner() *interner { return &interner{m: map[string]string{}} }

func (in *interner) s(b []byte) string {
	if s, ok := in.m[string(b)]; ok {
		return s
	}
	s := string(b)
	in.m[s] = s
	return s
}

func (p *parser) txt(i int) string {
	if i < 0 || i >= len(p.toks) {
		return ""
	}
	t := p.toks[i]
	return string(p.src[t.S:t.E])
}

func (p *parser) is(i int, s string) bool {
	if i < 0 || i >= len(p.toks) {
		return false
	}
	t := p.toks[i]
	return int(t.E-t.S) == len(s) && string(p.src[t.S:t.E]) == s
}

func (p *parser) ident(i int) bool {
	return i >= 0 && i < len(p.toks) && p.toks[i].Kind == tIdent
}

// col is the byte offset of t within its line.
func (p *parser) col(t token) int32 {
	ls := int(t.S)
	for ls > 0 && p.src[ls-1] != '\n' {
		ls--
	}
	return t.S - int32(ls)
}

// skipBalanced: toks[i] is an opener; returns the index after its closer.
func (p *parser) skipBalanced(i int) int {
	open := p.src[p.toks[i].S]
	var close byte
	switch open {
	case '(':
		close = ')'
	case '[':
		close = ']'
	case '{':
		close = '}'
	case '<':
		close = '>'
	}
	depth := 0
	for ; i < len(p.toks); i++ {
		t := p.toks[i]
		if t.Kind != tPunct {
			continue
		}
		c := p.src[t.S]
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return i + 1
			}
		} else if open == '<' && (c == ';' || c == '{' || c == '}') {
			return i // not a generic list after all
		}
	}
	return i
}

var modifiers = map[string]bool{
	"public": true, "private": true, "protected": true, "static": true, "final": true,
	"abstract": true, "native": true, "synchronized": true, "transient": true, "volatile": true,
	"strictfp": true, "default": true, "sealed": true, "non": true, "-": true,
}

// skipAnnotation: toks[i] is '@' not followed by 'interface'.
func (p *parser) skipAnnotation(i int) (int, string) {
	i++
	name := ""
	for p.ident(i) {
		name = p.txt(i)
		i++
		if p.is(i, ".") && p.ident(i+1) {
			i++
			continue
		}
		break
	}
	if p.is(i, "(") {
		i = p.skipBalanced(i)
	}
	return i, name
}

type annos struct {
	lombok                   uint8
	builder, logAt           pos
	noArgs, allArgs, reqArgs pos
}

func (p *parser) skipModifiers(i int) (int, annos) {
	an := annos{builder: noPos, logAt: noPos, noArgs: noPos, allArgs: noPos, reqArgs: noPos}
	for i < len(p.toks) {
		if p.is(i, "@") && !p.is(i+1, "interface") {
			if i+1 >= len(p.toks) { // a file that ends in '@'
				return i + 1, an
			}
			var name string
			at := p.toks[i+1]
			i, name = p.skipAnnotation(i)
			lb := &an.lombok
			switch name {
			case "Builder", "SuperBuilder":
				an.builder = pos{at.Line, p.col(at)}
			case "Slf4j", "Log4j2", "Log4j", "Log", "CommonsLog", "XSlf4j", "JBossLog", "Flogger":
				an.logAt = pos{at.Line, p.col(at)}
			case "NoArgsConstructor":
				an.noArgs = pos{at.Line, p.col(at)}
			case "AllArgsConstructor":
				an.allArgs = pos{at.Line, p.col(at)}
			case "RequiredArgsConstructor", "Data", "Value":
				if an.reqArgs.Line < 0 || name == "RequiredArgsConstructor" {
					an.reqArgs = pos{at.Line, p.col(at)}
				}
			}
			switch name {
			case "Data", "Getter", "Value":
				*lb |= lombokGetter
				if name == "Data" {
					*lb |= lombokSetter
				}
			case "Setter":
				*lb |= lombokSetter
			}
			continue
		}
		if p.ident(i) && modifiers[p.txt(i)] || p.is(i, "-") && p.is(i-1, "non") {
			i++
			continue
		}
		break
	}
	return i, an
}

// typeText renders the type in toks[a:b] as a dotted name without generics,
// annotations or array dimensions.
func (p *parser) typeText(a, b int) string {
	var sb []byte
	for i := a; i < b; i++ {
		if p.is(i, "@") {
			i, _ = p.skipAnnotation(i)
			i--
			continue
		}
		if p.is(i, "<") {
			i = p.skipBalanced(i) - 1
			continue
		}
		if p.ident(i) {
			s := p.txt(i)
			if modifiers[s] || s == "final" {
				continue
			}
			if len(sb) > 0 && sb[len(sb)-1] != '.' {
				sb = sb[:0] // "Foo Bar": keep the last name (defensive)
			}
			sb = append(sb, s...)
		} else if p.is(i, ".") {
			sb = append(sb, '.')
		}
	}
	return p.in.s(sb)
}

// ParseFile reads the declarations of one Java source file for the index.
// path is the repository-relative, slash-separated path the index reports
// back in a Location. It is safe to call from several goroutines at once.
func ParseFile(path string, src []byte) *File {
	return parseJava(src, path, newInterner(), false).f
}

func parseJava(src []byte, rel string, in *interner, keep bool) *parsed {
	p := &parser{src: src, toks: lex(src), in: in, keep: keep}
	p.f = &File{Path: rel, Imports: map[string]string{}}
	i := 0
	for i < len(p.toks) {
		if p.is(i, "@") && !p.is(i+1, "interface") {
			// Only package annotations belong to the header; class
			// annotations (@Data, @Getter) are left for the type.
			j := i
			for p.is(j, "@") && !p.is(j+1, "interface") {
				j, _ = p.skipAnnotation(j)
			}
			if !p.is(j, "package") {
				break
			}
			i = j
			continue
		}
		if p.is(i, "package") || p.is(i, "import") {
			isImport := p.is(i, "import")
			i++
			static := false
			if isImport && p.is(i, "static") {
				static = true
				i++
			}
			var sb []byte
			for i < len(p.toks) && !p.is(i, ";") {
				sb = append(sb, p.txt(i)...)
				i++
			}
			i++
			name := string(sb)
			switch {
			case !isImport:
				p.f.Pkg = p.in.s(sb)
			case static:
				p.f.Static = append(p.f.Static, p.in.s(sb))
			case strings.HasSuffix(name, ".*"):
				p.f.Wild = append(p.f.Wild, p.in.s(sb[:len(sb)-2]))
			default:
				p.f.Imports[p.in.s(sb[strings.LastIndexByte(name, '.')+1:])] = p.in.s(sb)
			}
			continue
		}
		break
	}
	p.parseBody(i, nil, false)
	return &parsed{f: p.f, bodies: p.bodies, ranges: p.ranges, tparams: p.tparams, toks: p.toks}
}

func (p *parser) isTypeKeyword(i int) bool {
	switch p.txt(i) {
	case "class", "interface", "enum":
		return p.ident(i + 1)
	case "record":
		return p.ident(i+1) && (p.is(i+2, "(") || p.is(i+2, "<"))
	}
	return p.is(i, "@") && p.is(i+1, "interface") && p.ident(i+2)
}

// parseBody parses members until the closing '}' of owner (or EOF at top level).
func (p *parser) parseBody(i int, owner *Type, enum bool) int {
	n := len(p.toks)
	if enum {
		i = p.parseEnumConstants(i, owner)
	}
	for i < n {
		if p.is(i, "}") {
			return i + 1
		}
		if p.is(i, ";") {
			i++
			continue
		}
		var an annos
		i, an = p.skipModifiers(i)
		lb := an.lombok
		if i >= n {
			break
		}
		if p.is(i, "{") { // initializer block
			e := p.skipBalanced(i)
			if owner != nil {
				p.addBody(owner, nil, i, e)
			}
			i = e
			continue
		}
		if p.isTypeKeyword(i) {
			i = p.parseTypeDecl(i, owner, an)
			continue
		}
		tpOpen := -1
		if p.is(i, "<") {
			tpOpen = i
			i = p.skipBalanced(i)
		}
		typeStart := i
		j, last := i, -1
		for j < n {
			switch {
			case p.is(j, "<"):
				j = p.skipBalanced(j)
			case p.is(j, "["):
				j = p.skipBalanced(j)
			case p.is(j, "@"):
				j, _ = p.skipAnnotation(j)
			case p.ident(j):
				last = j
				j++
			case p.is(j, ".") || p.is(j, "?") || p.is(j, "&") || p.is(j, "..."):
				j++
			default:
				goto stop
			}
		}
	stop:
		if j >= n {
			break
		}
		if last < 0 || owner == nil {
			if p.is(j, "{") {
				i = p.skipBalanced(j)
			} else {
				i = j + 1
			}
			continue
		}
		switch {
		case p.is(j, "("):
			m := &Member{Name: p.in.s([]byte(p.txt(last))), Owner: owner, Line: p.toks[last].Line, Col: p.col(p.toks[last])}
			if last == typeStart {
				m.Kind = kCtor
			} else {
				m.Kind = kMethod
				m.Type = p.typeText(typeStart, last)
			}
			pe := p.skipBalanced(j)
			m.Arity, m.Varargs = p.arity(j, pe)
			owner.Members = append(owner.Members, m)
			k := pe
			sawDefault := false
			for k < n {
				if p.is(k, "{") && !sawDefault {
					e := p.skipBalanced(k)
					p.addBody(owner, m, j, e)
					k = e
					break
				}
				if p.is(k, ";") {
					p.addBody(owner, m, j, k+1) // abstract: parameters still have a scope
					k++
					break
				}
				if p.is(k, "}") { // malformed; let the body loop close
					break
				}
				if p.is(k, "default") {
					sawDefault = true
				}
				if p.is(k, "(") || p.is(k, "{") || p.is(k, "[") {
					k = p.skipBalanced(k)
					continue
				}
				k++
			}
			p.addTypeParams(tpOpen, tpOpen, k)
			i = k
		case p.is(j, "=") || p.is(j, ";") || p.is(j, ","):
			ft := p.typeText(typeStart, last)
			k := j
			name := last
			for {
				p.addField(owner, name, ft, lb)
				if p.is(k, "=") {
					k = p.skipExpr(k + 1)
				}
				if p.is(k, ",") && p.ident(k+1) {
					name = k + 1
					k += 2
					for p.is(k, "[") {
						k = p.skipBalanced(k)
					}
					continue
				}
				break
			}
			if p.is(k, ";") {
				k++
			}
			i = k
		case p.is(j, "{"): // compact record constructor or something odd
			e := p.skipBalanced(j)
			p.addBody(owner, nil, j, e)
			i = e
		default:
			i = j + 1
		}
	}
	return i
}

func (p *parser) addField(owner *Type, at int, typ string, lb uint8) {
	owner.Members = append(owner.Members, &Member{
		Name: p.in.s([]byte(p.txt(at))), Kind: kField, Type: typ, Owner: owner,
		Line: p.toks[at].Line, Col: p.col(p.toks[at]), Lombok: lb,
	})
}

func (p *parser) addBody(owner *Type, m *Member, s, e int) {
	if p.keep {
		p.bodies = append(p.bodies, body{owner, m, s, e})
	}
}

// skipExpr skips an initializer up to the ',' or ';' that ends it.
func (p *parser) skipExpr(k int) int {
	for k < len(p.toks) {
		if p.is(k, "(") || p.is(k, "[") || p.is(k, "{") {
			k = p.skipBalanced(k)
			continue
		}
		// Type arguments: new HashMap<K, V>(), Foo.<T>bar(). An uppercase name
		// before '<' is a type; a comparison like a < b is left alone.
		if p.is(k, "<") && (p.is(k-1, ".") || p.ident(k-1) && isUpper(p.src[p.toks[k-1].S])) {
			if e := p.skipBalanced(k); p.is(e-1, ">") {
				k = e
				continue
			}
		}
		if p.is(k, ",") || p.is(k, ";") || p.is(k, "}") || p.is(k, ")") {
			return k
		}
		k++
	}
	return k
}

// arity counts top-level arguments in toks[open:end) (open is '(').
func (p *parser) arity(open, end int) (int16, bool) {
	if end-open <= 2 {
		return 0, false
	}
	n, depth, angle := int16(1), 0, 0
	varargs := false
	for k := open + 1; k < end-1; k++ {
		switch {
		case p.is(k, "(") || p.is(k, "[") || p.is(k, "{"):
			depth++
		case p.is(k, ")") || p.is(k, "]") || p.is(k, "}"):
			depth--
		case p.is(k, "<") && p.ident(k-1):
			angle++
		case p.is(k, ">") && angle > 0:
			angle--
		case p.is(k, "...") && depth == 0:
			varargs = true
		case p.is(k, ",") && depth == 0 && angle == 0:
			n++
		}
	}
	return n, varargs
}

func (p *parser) parseEnumConstants(i int, owner *Type) int {
	for i < len(p.toks) {
		if p.is(i, "@") {
			i, _ = p.skipAnnotation(i)
			continue
		}
		if p.is(i, ";") {
			return i + 1
		}
		if p.is(i, "}") {
			return i
		}
		if p.ident(i) {
			owner.Members = append(owner.Members, &Member{Name: p.in.s([]byte(p.txt(i))), Kind: kEnumConst,
				Type: owner.Name, Owner: owner, Line: p.toks[i].Line, Col: p.col(p.toks[i])})
			i++
			if p.is(i, "(") {
				i = p.skipBalanced(i)
			}
			if p.is(i, "{") {
				i = p.skipBalanced(i)
			}
			continue
		}
		i++
	}
	return i
}

func (p *parser) parseTypeDecl(i int, outer *Type, an annos) int {
	kw := p.txt(i)
	if kw == "@" {
		i++
		kw = "@interface"
	}
	nameTok := i + 1
	t := &Type{Name: p.in.s([]byte(p.txt(nameTok))), File: p.f, Outer: outer,
		Line: p.toks[nameTok].Line, Col: p.col(p.toks[nameTok]), Lombok: an.lombok,
		BuilderAt: an.builder, LogAt: an.logAt, NoArgsAt: an.noArgs, AllArgsAt: an.allArgs, ReqArgsAt: an.reqArgs}
	switch kw {
	case "class":
		t.Kind = kClass
	case "interface":
		t.Kind = kInterface
	case "enum":
		t.Kind = kEnum
	case "record":
		t.Kind = kRecord
	default:
		t.Kind = kAnnotation
	}
	if outer != nil {
		t.FQN = outer.FQN + "." + t.Name
		outer.Nested = append(outer.Nested, t)
	} else {
		if p.f.Pkg != "" {
			t.FQN = p.f.Pkg + "." + t.Name
		} else {
			t.FQN = t.Name
		}
		p.f.Types = append(p.f.Types, t)
	}
	j := nameTok + 1
	mode := ""
	for j < len(p.toks) && !p.is(j, "{") {
		switch {
		case p.is(j, "<"):
			j = p.skipBalanced(j)
		case p.is(j, "(") && t.Kind == kRecord && mode == "":
			e := p.skipBalanced(j)
			p.recordComponents(t, j, e)
			j = e
		case p.is(j, "extends") || p.is(j, "implements") || p.is(j, "permits"):
			mode = p.txt(j)
			j++
		case p.is(j, "@"):
			j, _ = p.skipAnnotation(j)
		case p.ident(j) && (mode == "extends" || mode == "implements"):
			s := j
			for p.ident(j) && p.is(j+1, ".") && p.ident(j+2) {
				j += 2
			}
			t.Supers = append(t.Supers, p.typeText(s, j+1))
			j++
		default:
			j++
		}
	}
	if j >= len(p.toks) {
		return j
	}
	e := p.parseBody(j+1, t, t.Kind == kEnum)
	p.addTypeParams(nameTok+1, nameTok, e)
	if p.keep {
		p.ranges = append(p.ranges, typeRange{t, j, e})
	}
	return e
}

func (p *parser) recordComponents(t *Type, open, end int) {
	s := open + 1
	depth := 0
	for k := open + 1; k < end; k++ {
		if p.is(k, "<") || p.is(k, "(") {
			depth++
		} else if p.is(k, ">") || p.is(k, ")") && k != end-1 {
			depth--
		}
		if depth == 0 && (p.is(k, ",") || k == end-1) {
			last := k - 1
			if p.ident(last) {
				p.addField(t, last, p.typeText(s, last), lombokGetter)
			}
			s = k + 1
		}
	}
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

type tparam struct {
	tok, s, e int // declaring token and scope token range
}

func (p *parser) addTypeParams(open, s, e int) {
	if !p.keep || !p.is(open, "<") {
		return
	}
	end := p.skipBalanced(open)
	depth := 0
	for k := open; k < end; k++ {
		switch {
		case p.is(k, "<"):
			depth++
		case p.is(k, ">"):
			depth--
		case depth == 1 && p.ident(k) && (p.is(k-1, "<") || p.is(k-1, ",")):
			p.tparams = append(p.tparams, tparam{k, s, e})
		}
	}
}
