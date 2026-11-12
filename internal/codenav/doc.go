// Package codenav answers "go to declaration" and "find usages" for Java
// sources without a language server.
//
// It keeps a resident index of declarations only (types, their supertypes as
// written, and their members with their declared types as written) and, for a
// query, parses just the file under the cursor to scope locals and infer the
// static type of a qualifier. Resolution follows Java's scoping rules for
// what source alone can decide: imports, packages, nesting, inheritance,
// blocks, call arity and the common Lombok annotations. It does not do type
// inference across expressions it cannot see (lambda parameters, generic
// substitution) or overload resolution by argument type; such names yield no
// answer rather than a guess. Names the working tree does not declare are
// looked up in a Library, the JDK's sources, parsed lazily one class at a
// time; library jars are not read. Usages are the occurrences in the working
// tree that navigate back to a declaration under those same rules. ADR 0040
// records the trade-off and how it was measured against JDTLS; ADR 0041 adds
// the JDK.
//
// The package is pure: it reads no files, starts no goroutines and imports
// no other Viff package. The app layer lists and reads files, supplies the
// Library's reader and owns the Index's lifecycle.
package codenav
