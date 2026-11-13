package deps

import "strings"

// Descriptors and generic signatures (JVMS 4.3, 4.7.9.1), read into a small
// type tree the stub writer renders as Java source.

// jtype is one Java type.
type jtype struct {
	prim  string      // a primitive or "void"; "" otherwise
	tvar  string      // a type variable
	parts []classPart // a class type: parts[0].name is the binary name, the others are member classes
	class string      // a class type's binary name, member classes joined with '$'
	dims  int
	wild  byte // as a type argument: '*' (unbounded), '+' (extends), '-' (super) or 0
}

type classPart struct {
	name string
	args []jtype
}

type typeParam struct {
	name   string
	bounds []jtype
}

// methodSig is a method's type parameters, parameter types and result.
type methodSig struct {
	tparams []typeParam
	params  []jtype
	ret     jtype
}

// classSig is a class's type parameters and supertypes.
type classSig struct {
	tparams    []typeParam
	super      jtype
	interfaces []jtype
}

type sigParser struct {
	s   string
	i   int
	bad bool
}

func (p *sigParser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *sigParser) expect(c byte) {
	if p.peek() != c {
		p.bad = true
		return
	}
	p.i++
}

func (p *sigParser) ident(stops string) string {
	s := p.i
	for p.i < len(p.s) && !strings.ContainsRune(stops, rune(p.s[p.i])) {
		p.i++
	}
	if p.i == s {
		p.bad = true
	}
	return p.s[s:p.i]
}

var basic = map[byte]string{'B': "byte", 'C': "char", 'D': "double", 'F': "float", 'I': "int", 'J': "long",
	'S': "short", 'Z': "boolean", 'V': "void"}

// javaType reads a field type: a primitive, a class or type variable, or an
// array of one.
func (p *sigParser) javaType() jtype {
	dims := 0
	for p.peek() == '[' {
		p.i++
		dims++
	}
	var t jtype
	switch c := p.peek(); c {
	case 'L':
		t = p.classType()
	case 'T':
		p.i++
		t = jtype{tvar: p.ident(";")}
		p.expect(';')
	default:
		name, ok := basic[c]
		if !ok || c == 'V' && dims > 0 {
			p.bad = true
			return jtype{}
		}
		p.i++
		t = jtype{prim: name}
	}
	t.dims = dims
	return t
}

// classType reads "Lpkg/Name<args>.Inner<args>;".
func (p *sigParser) classType() jtype {
	p.expect('L')
	var t jtype
	for !p.bad {
		name := p.ident("<.;")
		part := classPart{name: name}
		if p.peek() == '<' {
			part.args = p.typeArgs()
		}
		t.parts = append(t.parts, part)
		if p.peek() == '.' {
			p.i++
			continue
		}
		p.expect(';')
		break
	}
	names := make([]string, len(t.parts))
	for i, part := range t.parts {
		names[i] = part.name
	}
	t.class = strings.Join(names, "$")
	return t
}

func (p *sigParser) typeArgs() []jtype {
	p.expect('<')
	var out []jtype
	for !p.bad && p.peek() != '>' {
		switch c := p.peek(); c {
		case '*':
			p.i++
			out = append(out, jtype{wild: '*'})
		case '+', '-':
			p.i++
			t := p.javaType()
			t.wild = c
			out = append(out, t)
		default:
			out = append(out, p.javaType())
		}
		if p.i >= len(p.s) {
			p.bad = true
		}
	}
	p.expect('>')
	return out
}

func (p *sigParser) typeParams() []typeParam {
	if p.peek() != '<' {
		return nil
	}
	p.i++
	var out []typeParam
	for !p.bad && p.peek() != '>' {
		tp := typeParam{name: p.ident(":")}
		for !p.bad && p.peek() == ':' {
			p.i++
			if c := p.peek(); c == 'L' || c == 'T' || c == '[' {
				tp.bounds = append(tp.bounds, p.javaType())
			}
		}
		out = append(out, tp)
		if p.i >= len(p.s) {
			p.bad = true
		}
	}
	p.expect('>')
	return out
}

func (p *sigParser) done() bool { return !p.bad && p.i == len(p.s) }

func parseFieldDescriptor(s string) (jtype, error) {
	p := &sigParser{s: s}
	t := p.javaType()
	if !p.done() || t.prim == "void" {
		return jtype{}, errClassFormat
	}
	return t, nil
}

// parseMethodDescriptor reads "(params)result" (a descriptor or the
// parameter part of a signature without type parameters or throws).
func parseMethodDescriptor(s string) ([]jtype, jtype, error) {
	m, err := parseMethodSignature(s)
	return m.params, m.ret, err
}

func parseMethodSignature(s string) (methodSig, error) {
	p := &sigParser{s: s}
	var m methodSig
	m.tparams = p.typeParams()
	p.expect('(')
	for !p.bad && p.peek() != ')' {
		t := p.javaType()
		if t.prim == "void" {
			p.bad = true
		}
		m.params = append(m.params, t)
		if p.i >= len(p.s) {
			p.bad = true
		}
	}
	p.expect(')')
	m.ret = p.javaType()
	for !p.bad && p.peek() == '^' { // throws: not shown
		p.i++
		p.javaType()
	}
	if !p.done() {
		return methodSig{}, errClassFormat
	}
	return m, nil
}

func parseClassSignature(s string) (classSig, error) {
	p := &sigParser{s: s}
	var c classSig
	c.tparams = p.typeParams()
	c.super = p.classType()
	for !p.bad && p.peek() == 'L' {
		c.interfaces = append(c.interfaces, p.classType())
	}
	if !p.done() {
		return classSig{}, errClassFormat
	}
	return c, nil
}

// classOf is the type of a binary name, as the constant pool names classes.
func classOf(binary string) jtype {
	return jtype{parts: []classPart{{name: binary}}, class: binary}
}
