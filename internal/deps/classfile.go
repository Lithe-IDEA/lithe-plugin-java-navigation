package deps

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// A reader of the parts of a class file (JVMS chapter 4) that a signature
// stub needs: the class's name, flags, supertypes and generic signature, its
// fields and methods with their descriptors, signatures, parameter names,
// constant values and annotation defaults, its member classes and its record
// components. Method bodies and every other attribute are skipped.

// Access flags (JVMS 4.1, 4.5, 4.6, 4.7.6).
const (
	accPublic     = 0x0001
	accPrivate    = 0x0002
	accProtected  = 0x0004
	accStatic     = 0x0008
	accFinal      = 0x0010
	accBridge     = 0x0040
	accVarargs    = 0x0080
	accNative     = 0x0100
	accInterface  = 0x0200
	accAbstract   = 0x0400
	accSynthetic  = 0x1000
	accAnnotation = 0x2000
	accEnum       = 0x4000
)

var errClassFormat = errors.New("not a valid class file")

// classFile is what a stub is written from.
type classFile struct {
	access     uint16
	name       string // internal binary name: "a/b/Outer$Inner"
	super      string // "" for java/lang/Object and module-info
	interfaces []string
	signature  string // the Signature attribute, "" when absent
	fields     []member
	methods    []member
	inner      []innerClass
	record     []member // record components: name, desc, signature
}

type member struct {
	access    uint16
	name      string
	desc      string
	signature string
	params    []string // parameter names when the class file records them
	constant  string   // a field's ConstantValue as a Java literal, "" if none
	dflt      string   // an annotation element's default as Java source, "" if none
}

// innerClass is one entry of the InnerClasses attribute.
type innerClass struct {
	inner, outer, name string // outer and name are "" for local and anonymous classes
	access             uint16
}

type cpEntry struct {
	tag  byte
	a, b uint16 // indexes, or the two halves of a 4-byte value
	s    string // Utf8
	v    uint64 // Integer, Float, Long, Double bits
}

type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = errClassFormat
		return false
	}
	return true
}

func (r *reader) u1() byte {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

func (r *reader) u2() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.BigEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v
}

func (r *reader) u4() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}

func (r *reader) bytes(n int) []byte {
	if !r.need(n) {
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}

// parseClass reads b, the content of a .class file.
func parseClass(b []byte) (*classFile, error) {
	r := &reader{b: b}
	if r.u4() != 0xCAFEBABE {
		return nil, errClassFormat
	}
	r.u2() // minor
	r.u2() // major
	n := int(r.u2())
	cp := make([]cpEntry, n)
	for i := 1; i < n && r.err == nil; i++ {
		e := cpEntry{tag: r.u1()}
		switch e.tag {
		case 1:
			e.s = decodeModifiedUTF8(r.bytes(int(r.u2())))
		case 3, 4:
			e.v = uint64(r.u4())
		case 5, 6:
			e.v = uint64(r.u4())<<32 | uint64(r.u4())
		case 7, 8, 16, 19, 20:
			e.a = r.u2()
		case 9, 10, 11, 12, 17, 18:
			e.a, e.b = r.u2(), r.u2()
		case 15:
			r.u1()
			e.a = r.u2()
		default:
			return nil, fmt.Errorf("%w: constant pool tag %d", errClassFormat, e.tag)
		}
		cp[i] = e
		if e.tag == 5 || e.tag == 6 {
			i++ // eight-byte constants take two entries
		}
	}
	c := &classFile{}
	p := pool(cp)
	c.access = r.u2()
	c.name = p.class(r.u2())
	c.super = p.class(r.u2())
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		c.interfaces = append(c.interfaces, p.class(r.u2()))
	}
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		c.fields = append(c.fields, readMember(r, p, false))
	}
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		c.methods = append(c.methods, readMember(r, p, true))
	}
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		name := p.utf8(r.u2())
		body := &reader{b: r.bytes(int(r.u4()))}
		switch name {
		case "Signature":
			c.signature = p.utf8(body.u2())
		case "InnerClasses":
			for m := int(body.u2()); m > 0 && body.err == nil; m-- {
				ic := innerClass{inner: p.class(body.u2()), outer: p.class(body.u2()), name: p.utf8(body.u2())}
				ic.access = body.u2()
				c.inner = append(c.inner, ic)
			}
		case "Record":
			for m := int(body.u2()); m > 0 && body.err == nil; m-- {
				rc := member{name: p.utf8(body.u2()), desc: p.utf8(body.u2())}
				for a := int(body.u2()); a > 0 && body.err == nil; a-- {
					an := p.utf8(body.u2())
					ab := &reader{b: body.bytes(int(body.u4()))}
					if an == "Signature" {
						rc.signature = p.utf8(ab.u2())
					}
				}
				c.record = append(c.record, rc)
			}
		}
		if body.err != nil {
			return nil, body.err
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if c.name == "" {
		return nil, errClassFormat
	}
	return c, nil
}

func readMember(r *reader, p pool, method bool) member {
	m := member{access: r.u2(), name: p.utf8(r.u2()), desc: p.utf8(r.u2())}
	var mp, lvt []string
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		name := p.utf8(r.u2())
		body := &reader{b: r.bytes(int(r.u4()))}
		switch name {
		case "Signature":
			m.signature = p.utf8(body.u2())
		case "ConstantValue":
			if !method {
				m.constant = p.literal(body.u2(), m.desc)
			}
		case "MethodParameters":
			for n := int(body.u1()); n > 0 && body.err == nil; n-- {
				mp = append(mp, p.utf8(body.u2()))
				body.u2()
			}
		case "AnnotationDefault":
			m.dflt = elementValue(body, p, 0)
			if body.err != nil {
				m.dflt = ""
			}
		case "Code":
			lvt = paramsFromCode(body, p, m)
		}
	}
	if method {
		m.params = paramNames(mp, lvt, m)
	}
	return m
}

// paramsFromCode reads the LocalVariableTable of a Code attribute: classes
// compiled with debug information (Maven's default) name their parameters
// there. It returns the names by local variable slot, "" for unknown slots.
func paramsFromCode(r *reader, p pool, m member) []string {
	r.u2() // max_stack
	r.u2() // max_locals
	r.bytes(int(r.u4()))
	r.bytes(int(r.u2()) * 8) // exception table
	var slots []string
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		name := p.utf8(r.u2())
		body := &reader{b: r.bytes(int(r.u4()))}
		if name != "LocalVariableTable" {
			continue
		}
		for n := int(body.u2()); n > 0 && body.err == nil; n-- {
			start := body.u2()
			body.u2() // length
			vn := p.utf8(body.u2())
			body.u2() // descriptor
			slot := int(body.u2())
			if start != 0 || slot > 255 {
				continue
			}
			for len(slots) <= slot {
				slots = append(slots, "")
			}
			slots[slot] = vn
		}
	}
	return slots
}

// paramNames picks the parameter names of m: the MethodParameters attribute
// when it names every parameter, else the local variable slots of the
// parameters, else none.
func paramNames(mp, slots []string, m member) []string {
	params, _, err := parseMethodDescriptor(m.desc)
	if err != nil {
		return nil
	}
	if len(mp) == len(params) {
		ok := true
		for _, n := range mp {
			if !validIdent(n) {
				ok = false
			}
		}
		if ok {
			return mp
		}
	}
	if len(slots) == 0 {
		return nil
	}
	slot := 0
	if m.access&accStatic == 0 {
		slot = 1 // this
	}
	out := make([]string, len(params))
	for i, t := range params {
		if slot >= len(slots) || !validIdent(slots[slot]) {
			return nil
		}
		out[i] = slots[slot]
		slot++
		if t.dims == 0 && (t.prim == "long" || t.prim == "double") {
			slot++
		}
	}
	return out
}

func validIdent(s string) bool {
	if s == "" || javaKeyword[s] {
		return false
	}
	for i, c := range s {
		ok := c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c > 0x7f || i > 0 && c >= '0' && c <= '9'
		if !ok {
			return false
		}
	}
	return true
}

var javaKeyword = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`abstract assert boolean break byte case catch char class const continue
		default do double else enum extends final finally float for goto if implements import instanceof int
		interface long native new package private protected public return short static strictfp super switch
		synchronized this throw throws transient try void volatile while true false null _`) {
		m[w] = true
	}
	return m
}()

// elementValue renders an annotation element value (JVMS 4.7.16.1) as Java
// source; nested annotations, which a stub does not need, become "".
func elementValue(r *reader, p pool, depth int) string {
	if depth > 8 {
		r.err = errClassFormat
		return ""
	}
	tag := r.u1()
	switch tag {
	case 's': // the index names a Utf8 constant, not a String one
		return stringLiteral(p.utf8(r.u2()))
	case 'B', 'C', 'D', 'F', 'I', 'J', 'S', 'Z':
		return p.literal(r.u2(), string(tag))
	case 'e':
		t, name := p.utf8(r.u2()), p.utf8(r.u2())
		ty, err := parseFieldDescriptor(t)
		if err != nil {
			return ""
		}
		return "\x00" + ty.class + "\x00." + name // the type is rendered by the stub writer
	case 'c':
		t, err := parseFieldDescriptor(p.utf8(r.u2()))
		if err != nil {
			return ""
		}
		if t.class == "" {
			return t.prim + strings.Repeat("[]", t.dims) + ".class"
		}
		return "\x00" + t.class + "\x00" + strings.Repeat("[]", t.dims) + ".class"
	case '@':
		skipAnnotation(r, p, depth+1)
		return ""
	case '[':
		var parts []string
		for k := int(r.u2()); k > 0 && r.err == nil; k-- {
			v := elementValue(r, p, depth+1)
			if v == "" {
				parts = nil
				for ; k > 1 && r.err == nil; k-- {
					elementValue(r, p, depth+1)
				}
				return ""
			}
			parts = append(parts, v)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	r.err = errClassFormat
	return ""
}

func skipAnnotation(r *reader, p pool, depth int) {
	r.u2()
	for k := int(r.u2()); k > 0 && r.err == nil; k-- {
		r.u2()
		elementValue(r, p, depth)
	}
}

type pool []cpEntry

func (p pool) at(i uint16, tag byte) (cpEntry, bool) {
	if int(i) <= 0 || int(i) >= len(p) || p[i].tag != tag {
		return cpEntry{}, false
	}
	return p[i], true
}

func (p pool) utf8(i uint16) string {
	e, _ := p.at(i, 1)
	return e.s
}

func (p pool) class(i uint16) string {
	e, ok := p.at(i, 7)
	if !ok {
		return ""
	}
	return p.utf8(e.a)
}

// literal renders constant i as a Java literal of the type desc names.
func (p pool) literal(i uint16, desc string) string {
	if int(i) <= 0 || int(i) >= len(p) {
		return ""
	}
	e := p[i]
	switch e.tag {
	case 3:
		v := int32(uint32(e.v))
		switch desc {
		case "Z":
			return strconv.FormatBool(v != 0)
		case "C":
			return charLiteral(rune(v))
		case "B":
			return "(byte) " + strconv.Itoa(int(v))
		case "S":
			return "(short) " + strconv.Itoa(int(v))
		}
		return strconv.Itoa(int(v))
	case 4:
		return floatLiteral(float64(math.Float32frombits(uint32(e.v))), 32, "F")
	case 5:
		return strconv.FormatInt(int64(e.v), 10) + "L"
	case 6:
		return floatLiteral(math.Float64frombits(e.v), 64, "")
	case 8:
		return stringLiteral(p.utf8(e.a))
	}
	return ""
}

func floatLiteral(f float64, bits int, suffix string) string {
	switch {
	case math.IsNaN(f):
		if bits == 32 {
			return "0.0F / 0.0F"
		}
		return "0.0 / 0.0"
	case math.IsInf(f, 1):
		return "1.0" + suffix + " / 0.0" + suffix
	case math.IsInf(f, -1):
		return "-1.0" + suffix + " / 0.0" + suffix
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s + suffix
}

func stringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		b.WriteString(escapeRune(c, '"'))
	}
	b.WriteByte('"')
	return b.String()
}

func charLiteral(c rune) string { return "'" + escapeRune(c, '\'') + "'" }

func escapeRune(c rune, quote rune) string {
	switch c {
	case '\\':
		return `\\`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case quote:
		return `\` + string(quote)
	}
	if c < 0x20 || c == 0x7f || c >= 0xd800 && c < 0xe000 || c == 0x2028 || c == 0x2029 {
		return fmt.Sprintf(`\u%04x`, c)
	}
	return string(c)
}

// decodeModifiedUTF8 decodes the class file's string encoding (JVMS 4.4.7):
// UTF-8 with NUL as two bytes and supplementary characters as two encoded
// surrogates.
func decodeModifiedUTF8(b []byte) string {
	ascii := true
	for _, c := range b {
		if c >= 0x80 || c == 0 {
			ascii = false
			break
		}
	}
	if ascii {
		return string(b)
	}
	u := make([]uint16, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			u = append(u, uint16(c))
			i++
		case c&0xE0 == 0xC0 && i+1 < len(b):
			u = append(u, uint16(c&0x1F)<<6|uint16(b[i+1]&0x3F))
			i += 2
		case c&0xF0 == 0xE0 && i+2 < len(b):
			u = append(u, uint16(c&0x0F)<<12|uint16(b[i+1]&0x3F)<<6|uint16(b[i+2]&0x3F))
			i += 3
		default:
			u = append(u, 0xFFFD)
			i++
		}
	}
	return string(utf16.Decode(u))
}
