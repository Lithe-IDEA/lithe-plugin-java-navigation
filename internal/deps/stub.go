package deps

import (
	"sort"
	"strconv"
	"strings"
)

// Signature stubs: Java source written from the class files of a jar that
// has no sources attached. A stub declares what the class file declares and
// navigation can reach (the class and its member classes, their supertypes,
// and their non-private fields, constructors and methods with parameter
// names when the class file records them), so codenav parses it like any
// other library source and go to definition lands on a readable declaration.
// Method bodies are "{ /* compiled code */ }". The output is a function of
// the class files alone, so the copy an editor opens and the text codenav
// parsed agree on every position.

// maxNesting bounds member classes inside member classes.
const maxNesting = 8

// stubHeader starts every stub; origin names the jar.
func stubHeader(origin string) string {
	return "// Signatures read from " + origin + " by Java Support Lite; no sources are attached.\n"
}

type stubWriter struct {
	top     *classFile
	pkg     string // internal form: "a/b"
	load    func(binary string) (*classFile, bool)
	exists  func(binary string) bool // a class of this jar
	inner   map[string]innerClass    // every InnerClasses entry seen, by inner binary name
	own     map[string]bool          // simple names of the top class and its member classes
	refs    map[string]bool          // top-level binary names referenced
	b       strings.Builder
	indent  int
	written int
}

// writeStub returns the stub of top, a top-level class. load reads another
// class of the same jar by binary name; exists reports whether the jar has
// one without reading it.
func writeStub(top *classFile, origin string, load func(binary string) (*classFile, bool), exists func(binary string) bool) []byte {
	w := &stubWriter{top: top, load: load, exists: exists, inner: map[string]innerClass{},
		own: map[string]bool{}, refs: map[string]bool{}}
	if i := strings.LastIndexByte(top.name, '/'); i >= 0 {
		w.pkg = top.name[:i]
	}
	w.own[simpleOf(top.name)] = true
	w.class(top, top.access, 0)
	body := w.b.String()

	names := w.names()
	var out strings.Builder
	out.WriteString(stubHeader(origin))
	if w.pkg != "" {
		out.WriteString("package " + strings.ReplaceAll(w.pkg, "/", ".") + ";\n")
	}
	var imports []string
	for bin, name := range names {
		if !strings.Contains(name, ".") && w.packageOf(bin) != w.pkg && w.packageOf(bin) != "java/lang" {
			imports = append(imports, dotted(bin))
		}
	}
	sort.Strings(imports)
	if len(imports) > 0 {
		out.WriteByte('\n')
		for _, im := range imports {
			out.WriteString("import " + im + ";\n")
		}
	}
	out.WriteByte('\n')
	// Replace the placeholders "\x00binary\x00" with the chosen names.
	for {
		i := strings.IndexByte(body, 0)
		if i < 0 {
			out.WriteString(body)
			break
		}
		j := strings.IndexByte(body[i+1:], 0)
		if j < 0 {
			out.WriteString(body)
			break
		}
		out.WriteString(body[:i])
		out.WriteString(w.render(body[i+1:i+1+j], names))
		body = body[i+2+j:]
	}
	return []byte(out.String())
}

func simpleOf(binary string) string {
	return binary[strings.LastIndexAny(binary, "/$")+1:]
}

func dotted(binary string) string { return strings.ReplaceAll(binary, "/", ".") }

func (w *stubWriter) packageOf(binary string) string {
	if i := strings.LastIndexByte(binary, '/'); i >= 0 {
		return binary[:i]
	}
	return ""
}

// topOf splits a binary name into its top-level class and the member class
// path below it, as InnerClasses entries describe them; without an entry a
// '$' is taken as a member class separator, which is what javac emits.
func (w *stubWriter) topOf(binary string) (string, []string) {
	var path []string
	for b := binary; ; {
		if ic, ok := w.inner[b]; ok && ic.outer != "" && ic.name != "" {
			path = append([]string{ic.name}, path...)
			b = ic.outer
			continue
		}
		pkg := w.packageOf(b)
		rest := b[len(pkg):]
		if pkg != "" {
			rest = rest[1:]
		}
		segs := strings.Split(rest, "$")
		if len(segs) > 1 && segs[0] != "" && !strings.Contains(rest, "$$") {
			path = append(segs[1:], path...)
			b = b[:len(b)-len(rest)] + segs[0]
		}
		return b, path
	}
}

// names decides how each referenced top-level class is written: its simple
// name when that is unambiguous (the class itself, its package, java.lang,
// or an import), else its fully qualified name.
func (w *stubWriter) names() map[string]string {
	out := map[string]string{}
	bySimple := map[string][]string{}
	for bin := range w.refs {
		bySimple[simpleOf(bin)] = append(bySimple[simpleOf(bin)], bin)
	}
	for simple, bins := range bySimple {
		sort.Strings(bins)
		for k, bin := range bins {
			full := dotted(bin)
			switch {
			case bin == w.top.name:
				out[bin] = simple
			case w.own[simple]:
				// A member class of the same name would shadow it.
				out[bin] = full
			case len(bins) > 1 && k > 0:
				out[bin] = full
			case w.packageOf(bin) == "java/lang" && w.exists(w.pkg+"/"+simple):
				// The stub's own package declares the name.
				out[bin] = full
			case strings.Contains(simple, "$"):
				out[bin] = full
			default:
				out[bin] = simple
			}
		}
	}
	return out
}

func (w *stubWriter) render(binary string, names map[string]string) string {
	top, path := w.topOf(binary)
	s := names[top]
	if s == "" {
		s = dotted(top)
	}
	for _, p := range path {
		s += "." + p
	}
	return s
}

// ref returns the placeholder of a class and records its top-level class.
func (w *stubWriter) ref(binary string) string {
	top, _ := w.topOf(binary)
	w.refs[top] = true
	return "\x00" + binary + "\x00"
}

func (w *stubWriter) line(s string) {
	w.b.WriteString(strings.Repeat("    ", w.indent))
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *stubWriter) typ(t jtype) string {
	var s string
	switch {
	case t.wild == '*':
		return "?"
	case t.prim != "":
		s = t.prim
	case t.tvar != "":
		s = t.tvar
	case len(t.parts) > 0:
		for i, p := range t.parts {
			if i == 0 {
				s = w.ref(p.name)
			} else {
				s += "." + p.name
			}
			if len(p.args) > 0 {
				args := make([]string, len(p.args))
				for k, a := range p.args {
					args[k] = w.typ(a)
				}
				s += "<" + strings.Join(args, ", ") + ">"
			}
		}
	default:
		s = "Object"
	}
	s += strings.Repeat("[]", t.dims)
	switch t.wild {
	case '+':
		return "? extends " + s
	case '-':
		return "? super " + s
	}
	return s
}

func (w *stubWriter) typeParams(tps []typeParam) string {
	if len(tps) == 0 {
		return ""
	}
	parts := make([]string, len(tps))
	for i, tp := range tps {
		s := tp.name
		var bounds []string
		for _, b := range tp.bounds {
			if b.class == "java/lang/Object" && len(tp.bounds) == 1 {
				continue
			}
			bounds = append(bounds, w.typ(b))
		}
		if len(bounds) > 0 {
			s += " extends " + strings.Join(bounds, " & ")
		}
		parts[i] = s
	}
	return "<" + strings.Join(parts, ", ") + ">"
}

// class writes c, whose access flags are access (a member class's come
// from its outer class's InnerClasses entry).
func (w *stubWriter) class(c *classFile, access uint16, depth int) {
	for _, ic := range c.inner {
		if _, ok := w.inner[ic.inner]; !ok {
			w.inner[ic.inner] = ic
		}
	}
	var members []innerClass
	for _, ic := range c.inner {
		if ic.outer == c.name && ic.name != "" && ic.access&(accPrivate|accSynthetic) == 0 && validIdent(ic.name) {
			members = append(members, ic)
			w.own[ic.name] = true
		}
	}
	simple := simpleOf(c.name)
	if depth > 0 {
		if ic, ok := w.inner[c.name]; ok && ic.name != "" {
			simple = ic.name
		}
	}
	kind := "class"
	isRecord := c.super == "java/lang/Record" && c.record != nil
	switch {
	case access&accAnnotation != 0:
		kind = "@interface"
	case access&accInterface != 0:
		kind = "interface"
	case access&accEnum != 0 && c.super == "java/lang/Enum":
		kind = "enum"
	case isRecord:
		kind = "record"
	}
	var mods []string
	mods = append(mods, visibility(access)...)
	if depth > 0 && access&accStatic != 0 && kind == "class" {
		mods = append(mods, "static")
	}
	if access&accAbstract != 0 && kind == "class" {
		mods = append(mods, "abstract")
	}
	if access&accFinal != 0 && kind == "class" {
		mods = append(mods, "final")
	}
	head := strings.Join(append(mods, kind, simple), " ")

	sig := classSig{super: classOf(c.super)}
	for _, i := range c.interfaces {
		sig.interfaces = append(sig.interfaces, classOf(i))
	}
	if c.signature != "" {
		if s, err := parseClassSignature(c.signature); err == nil {
			sig = s
		}
	}
	head += w.typeParams(sig.tparams)
	if isRecord {
		comps := make([]string, len(c.record))
		for i, rc := range c.record {
			comps[i] = w.fieldType(rc) + " " + rc.name
		}
		head += "(" + strings.Join(comps, ", ") + ")"
	}
	if kind == "class" && c.super != "" && c.super != "java/lang/Object" {
		head += " extends " + w.typ(sig.super)
	}
	var ifaces []string
	for _, t := range sig.interfaces {
		if kind == "@interface" && t.class == "java/lang/annotation/Annotation" {
			continue
		}
		ifaces = append(ifaces, w.typ(t))
	}
	if len(ifaces) > 0 {
		if kind == "interface" {
			head += " extends "
		} else {
			head += " implements "
		}
		head += strings.Join(ifaces, ", ")
	}
	w.line(head + " {")
	w.indent++

	if kind == "enum" {
		var consts []string
		for _, f := range c.fields {
			if f.access&accEnum != 0 && validIdent(f.name) {
				consts = append(consts, f.name)
			}
		}
		for i, n := range consts {
			if i == len(consts)-1 {
				w.line(n + ";")
			} else {
				w.line(n + ",")
			}
		}
		if len(consts) == 0 {
			w.line(";")
		}
	}
	for _, f := range c.fields {
		if f.access&(accPrivate|accSynthetic) != 0 || f.access&accEnum != 0 || !validIdent(f.name) {
			continue
		}
		if isRecord && f.access&accStatic == 0 {
			continue // the components
		}
		var fm []string
		if kind != "interface" && kind != "@interface" {
			fm = visibility(f.access)
			if f.access&accStatic != 0 {
				fm = append(fm, "static")
			}
			if f.access&accFinal != 0 {
				fm = append(fm, "final")
			}
		}
		s := strings.Join(append(fm, w.fieldType(f), f.name), " ")
		if f.constant != "" {
			s += " = " + f.constant
		}
		w.line(s + ";")
	}
	outerParam := ""
	if depth > 0 && access&accStatic == 0 && kind == "class" {
		if ic, ok := w.inner[c.name]; ok {
			outerParam = ic.outer
		}
	}
	for _, m := range c.methods {
		w.method(c, kind, simple, m, outerParam)
	}
	if depth < maxNesting {
		for _, ic := range members {
			nc, ok := w.load(ic.inner)
			if !ok {
				continue
			}
			w.class(nc, ic.access, depth+1)
		}
	}
	w.indent--
	w.line("}")
}

func visibility(access uint16) []string {
	switch {
	case access&accPublic != 0:
		return []string{"public"}
	case access&accProtected != 0:
		return []string{"protected"}
	}
	return nil
}

func (w *stubWriter) fieldType(f member) string {
	if f.signature != "" {
		p := &sigParser{s: f.signature}
		if t := p.javaType(); p.done() {
			return w.typ(t)
		}
	}
	t, err := parseFieldDescriptor(f.desc)
	if err != nil {
		return "Object"
	}
	return w.typ(t)
}

func (w *stubWriter) method(c *classFile, kind, simple string, m member, outerParam string) {
	if m.access&(accPrivate|accSynthetic|accBridge) != 0 || m.name == "<clinit>" {
		return
	}
	ctor := m.name == "<init>"
	if !ctor && !validIdent(m.name) {
		return
	}
	if kind == "enum" && (ctor || m.name == "values" && strings.HasPrefix(m.desc, "()") ||
		m.name == "valueOf" && strings.HasPrefix(m.desc, "(Ljava/lang/String;)")) {
		return
	}
	if kind == "record" {
		if ctor && m.desc == recordCtorDesc(c) {
			return
		}
		if strings.HasPrefix(m.desc, "()") {
			for _, rc := range c.record {
				if rc.name == m.name {
					return
				}
			}
		}
		// The members every record has, which javac generates final.
		if m.access&accFinal != 0 && (m.name == "toString" && m.desc == "()Ljava/lang/String;" ||
			m.name == "hashCode" && m.desc == "()I" || m.name == "equals" && m.desc == "(Ljava/lang/Object;)Z") {
			return
		}
	}
	desc, err := parseMethodSignature(m.desc)
	if err != nil {
		return
	}
	names := m.params
	if outerParam != "" && ctor && len(desc.params) > 0 && desc.params[0].class == outerParam && desc.params[0].dims == 0 {
		// An inner class's constructor takes its outer instance first.
		desc.params = desc.params[1:]
		if len(names) > 0 {
			names = names[1:]
		}
	}
	sig := desc
	if m.signature != "" {
		if s, err := parseMethodSignature(m.signature); err == nil && len(s.params) <= len(desc.params) {
			sig = s
		}
	}
	if len(names) > len(sig.params) {
		names = names[len(names)-len(sig.params):]
	}
	var mods []string
	body := " { /* compiled code */ }"
	switch kind {
	case "interface", "@interface":
		switch {
		case m.access&accStatic != 0:
			mods = append(mods, "static")
		case m.access&accAbstract == 0:
			mods = append(mods, "default")
		default:
			body = ";"
		}
	default:
		mods = visibility(m.access)
		if m.access&accStatic != 0 {
			mods = append(mods, "static")
		}
		if m.access&accFinal != 0 {
			mods = append(mods, "final")
		}
		if m.access&accAbstract != 0 {
			mods = append(mods, "abstract")
			body = ";"
		}
		if m.access&accNative != 0 {
			mods = append(mods, "native")
			body = ";"
		}
	}
	if tp := w.typeParams(sig.tparams); tp != "" {
		mods = append(mods, tp)
	}
	params := make([]string, len(sig.params))
	for i, t := range sig.params {
		ts := ""
		if i == len(sig.params)-1 && m.access&accVarargs != 0 && t.dims > 0 {
			t.dims--
			ts = w.typ(t) + "..."
		} else {
			ts = w.typ(t)
		}
		name := "arg" + strconv.Itoa(i)
		if i < len(names) && validIdent(names[i]) {
			name = names[i]
		}
		params[i] = ts + " " + name
	}
	s := strings.Join(mods, " ")
	if s != "" {
		s += " "
	}
	if ctor {
		s += simple
	} else {
		s += w.typ(sig.ret) + " " + m.name
	}
	s += "(" + strings.Join(params, ", ") + ")"
	if kind == "@interface" && m.dflt != "" {
		s += " default " + w.value(m.dflt)
		body = ";"
	}
	w.line(s + body)
}

// value renders an annotation default, whose class names are placeholders
// of their binary names already ("\x00a/b/C\x00").
func (w *stubWriter) value(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] != 0 {
			continue
		}
		j := strings.IndexByte(v[i+1:], 0)
		if j < 0 {
			break
		}
		w.ref(v[i+1 : i+1+j])
		i += j + 1
	}
	return v
}

func recordCtorDesc(c *classFile) string {
	var b strings.Builder
	b.WriteByte('(')
	for _, rc := range c.record {
		b.WriteString(rc.desc)
	}
	b.WriteString(")V")
	return b.String()
}
