package codenav

// This file is a Lithe addition: qualified names the upstream resolver did
// not follow, found by comparing against JDT LS on Nacos.

// qualifierDot returns the '.' or '::' that qualifies the name at ti,
// looking past explicit type arguments (Foo.<T>bar(), Foo::<T>bar), or -1.
func (q *qctx) qualifierDot(ti int) int {
	p := q.p
	i := ti - 1
	if p.is(i, ">") {
		d := 0
		for ; i >= 0; i-- {
			switch {
			case p.is(i, ">"):
				d++
			case p.is(i, "<"):
				d--
			case p.ident(i) || p.is(i, ".") || p.is(i, ",") || p.is(i, "?") || p.is(i, "[") || p.is(i, "]") || p.is(i, "&") || p.is(i, "@"):
			default:
				return -1
			}
			if d == 0 {
				break
			}
		}
		i--
	}
	if p.is(i, ".") || p.is(i, "::") {
		return i
	}
	return -1
}

// annotationName reports that the name after dot ends an annotation's
// qualified type name, as Inner in @Outer.Inner(...).
func (q *qctx) annotationName(dot int) bool {
	p := q.p
	if !p.is(dot, ".") {
		return false
	}
	for k := dot - 1; p.ident(k); k -= 2 {
		if p.is(k-1, "@") {
			return true
		}
		if !p.is(k-1, ".") {
			return false
		}
	}
	return false
}
