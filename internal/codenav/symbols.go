package codenav

// This file is a Lithe addition; the rest of the package is copied from Viff
// unchanged (see UPSTREAM.md). Keeping additions in their own files lets the
// package be synced again by copying.

// SymbolKind is what a Symbol declares.
type SymbolKind uint8

const (
	SymbolClass SymbolKind = iota
	SymbolInterface
	SymbolEnum
	SymbolRecord
	SymbolAnnotation
	SymbolMethod
	SymbolConstructor
	SymbolField
	SymbolEnumConstant
)

// Symbol is one declaration of a file, for an outline: a type with its
// members and nested types, or a member. Line and Column locate the declared
// name (0-based line, byte column).
type Symbol struct {
	Name         string
	Kind         SymbolKind
	Detail       string // a member's declared type as written; "" otherwise
	Line, Column int
	Children     []Symbol
}

// Symbols lists the declarations of src, the content of a Java file, in
// source order. Lombok-generated members are not listed: they have no source
// of their own.
func Symbols(src []byte) []Symbol {
	f := ParseFile("", src)
	out := make([]Symbol, 0, len(f.Types))
	for _, t := range f.Types {
		out = append(out, typeSymbol(t))
	}
	return out
}

func typeSymbol(t *Type) Symbol {
	s := Symbol{Name: t.Name, Kind: typeKind(t.Kind), Line: int(t.Line), Column: int(t.Col)}
	children := make([]Symbol, 0, len(t.Members)+len(t.Nested))
	for _, m := range t.Members {
		if m.Ret != nil || m.Line < 0 {
			continue // synthetic (Lombok) member
		}
		children = append(children, Symbol{Name: m.Name, Kind: memberKind(m.Kind), Detail: m.Type, Line: int(m.Line), Column: int(m.Col)})
	}
	for _, n := range t.Nested {
		children = append(children, typeSymbol(n))
	}
	sortSymbols(children)
	s.Children = children
	return s
}

// sortSymbols orders symbols by position; members and nested types are
// collected separately but interleave in the source.
func sortSymbols(s []Symbol) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && before(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func before(a, b Symbol) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Column < b.Column
}

func typeKind(k declKind) SymbolKind {
	switch k {
	case kInterface:
		return SymbolInterface
	case kEnum:
		return SymbolEnum
	case kRecord:
		return SymbolRecord
	case kAnnotation:
		return SymbolAnnotation
	default:
		return SymbolClass
	}
}

func memberKind(k declKind) SymbolKind {
	switch k {
	case kCtor:
		return SymbolConstructor
	case kField:
		return SymbolField
	case kEnumConst:
		return SymbolEnumConstant
	default:
		return SymbolMethod
	}
}
