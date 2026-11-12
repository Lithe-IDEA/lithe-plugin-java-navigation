package codenav

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// A miniature JDK laid out like a JDK 9+ src.zip: module directories, then
// packages. Only what the tests navigate through is declared.
var jdk = map[string]string{
	"java.base/java/lang/Object.java": `package java.lang;

public class Object {
    public native int hashCode();
    public boolean equals(Object obj) { return this == obj; }
    public String toString() { return null; }
}
`,
	"java.base/java/lang/String.java": `package java.lang;

public final class String implements CharSequence {
    private final byte[] value;

    public int length() {
        int n = value.length;
        return n;
    }

    public String trim() { return this; }
}
`,
	"java.base/java/lang/CharSequence.java": `package java.lang;

public interface CharSequence {
    int length();
}
`,
	"java.base/java/lang/Enum.java": `package java.lang;

public abstract class Enum<E extends Enum<E>> {
    public final String name() { return null; }
}
`,
	"java.base/java/util/Collection.java": `package java.util;

import java.util.stream.Stream;

public interface Collection<E> {
    int size();
    default Stream<E> stream() { return null; }
}
`,
	"java.base/java/util/List.java": `package java.util;

public interface List<E> extends Collection<E> {
    E get(int index);
}
`,
	"java.base/java/util/Map.java": `package java.util;

public interface Map<K, V> {
    interface Entry<K, V> {
        K getKey();
    }
}
`,
	"java.base/java/util/stream/Stream.java": `package java.util.stream;

public interface Stream<T> {
    long count();
}
`,
	"java.base/module-info.java":            "module java.base {}\n",
	"java.base/java/lang/package-info.java": "package java.lang;\n",
}

// jdkPaths are jdk's paths in archive order.
func jdkPaths() []string {
	var out []string
	for p := range jdk {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// newJDK returns a library over jdk and the paths read so far.
func newJDK() (*Library, func() []string) {
	var mu sync.Mutex
	var read []string
	lib := NewLibrary(jdkPaths(), func(p string) ([]byte, error) {
		mu.Lock()
		read = append(read, p)
		mu.Unlock()
		src, ok := jdk[p]
		if !ok {
			return nil, fmt.Errorf("no entry %s", p)
		}
		return []byte(src), nil
	})
	return lib, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), read...)
	}
}

const useJDKPath = "src/com/acme/UseJdk.java"

const useJDK = `package com.acme;

import java.util.List;
import java.util.Map;

public class UseJdk {
    enum Color { RED }

    int run(String text, List<String> items, Map.Entry<String, Integer> e) {
        String t = text.trim();
        Object o = items.stream();
        int h = this.hashCode();
        String n = Color.RED.name();
        e.getKey();
        return t.length() + items.size();
    }
}
`

func TestLibraryFQN(t *testing.T) {
	for _, c := range []struct {
		entry, want string
		ok          bool
	}{
		{"java.base/java/lang/String.java", "java.lang.String", true},
		{"java/lang/String.java", "java.lang.String", true}, // JDK 8: no module directory
		{"jdk.compiler/com/sun/tools/javac/Main.java", "com.sun.tools.javac.Main", true},
		{"java.base/module-info.java", "", false},
		{"java.base/java/lang/package-info.java", "", false},
		{"java.base/java/lang/String.class", "", false},
		{"META-INF/MANIFEST.MF", "", false},
		{".java", "", false},
	} {
		got, ok := LibraryFQN(c.entry)
		if got != c.want || ok != c.ok {
			t.Errorf("LibraryFQN(%q) = %q, %v; want %q, %v", c.entry, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveIntoLibrary(t *testing.T) {
	ix := NewIndex()
	lib, read := newJDK()
	ix.SetLibrary(lib)
	ix.Put(ParseFile(useJDKPath, []byte(useJDK)))
	at := func(path, needle string) Location {
		l := declAtSrc(t, path, jdk[path], needle)
		l.Library = true
		return l
	}
	cases := []struct {
		click string
		nth   int
		want  Location
	}{
		// java.lang is imported implicitly.
		{"String text", 0, at("java.base/java/lang/String.java", "String implements")},
		{"trim()", 0, at("java.base/java/lang/String.java", "trim()")},
		{"length()", 0, at("java.base/java/lang/String.java", "length()")},
		// An explicit import, a method inherited from a library supertype,
		// and a nested library type.
		{"List<String>", 0, at("java.base/java/util/List.java", "List<E>")},
		{"stream()", 0, at("java.base/java/util/Collection.java", "stream()")},
		{"size()", 0, at("java.base/java/util/Collection.java", "size()")},
		{"Entry<String", 0, at("java.base/java/util/Map.java", "Entry<K, V>")},
		{"getKey()", 0, at("java.base/java/util/Map.java", "getKey()")},
		// Members every type inherits: Object's, and Enum's for an enum.
		{"hashCode()", 0, at("java.base/java/lang/Object.java", "hashCode()")},
		{"name()", 0, at("java.base/java/lang/Enum.java", "name()")},
		{"Object o", 0, at("java.base/java/lang/Object.java", "Object {")},
	}
	for _, c := range cases {
		got, ok := ix.Resolve(useJDKPath, []byte(useJDK), offsetOf(t, useJDK, c.click, c.nth, 0))
		got.rule, got.Name = "", ""
		if !ok || got != c.want {
			t.Errorf("%q resolved to %+v ok=%v, want %+v", c.click, got, ok, c.want)
		}
	}
	// Only the sources navigated through were read, each once; the
	// archive's other entries never were.
	seen := map[string]int{}
	for _, p := range read() {
		seen[p]++
		if seen[p] > 1 {
			t.Errorf("%s read %d times", p, seen[p])
		}
	}
	if seen["java.base/module-info.java"] > 0 || seen["java.base/java/util/stream/Stream.java"] > 0 {
		t.Errorf("read sources nobody navigated to: %v", read())
	}
	if lib.Len() != len(seen) {
		t.Errorf("library holds %d sources, read %d", lib.Len(), len(seen))
	}
}

func TestWorkingTreeShadowsLibrary(t *testing.T) {
	ix := NewIndex()
	lib, _ := newJDK()
	ix.SetLibrary(lib)
	// The package's own String wins over java.lang's, as in Java.
	own := "package com.acme;\n\npublic class String {}\n"
	use := "package com.acme;\n\nclass Use { String s; Integer i; }\n"
	ix.Put(ParseFile("src/com/acme/String.java", []byte(own)))
	ix.Put(ParseFile("src/com/acme/Use.java", []byte(use)))
	got, ok := ix.Resolve("src/com/acme/Use.java", []byte(use), offsetOf(t, use, "String s", 0, 0))
	if !ok || got.Library || got.Path != "src/com/acme/String.java" {
		t.Errorf("String resolved to %+v ok=%v, want the package's own", got, ok)
	}
	// A java.lang name the library lacks is still nothing.
	if got, ok := ix.Resolve("src/com/acme/Use.java", []byte(use), offsetOf(t, use, "Integer", 0, 0)); ok {
		t.Errorf("Integer resolved to %+v", got)
	}
	// Without a library, java.lang names resolve to nothing as before.
	ix.SetLibrary(nil)
	src := "package com.acme;\n\nclass Other { Object o; }\n"
	ix.Put(ParseFile("src/com/acme/Other.java", []byte(src)))
	if got, ok := ix.Resolve("src/com/acme/Other.java", []byte(src), offsetOf(t, src, "Object", 0, 0)); ok {
		t.Errorf("Object resolved to %+v without a library", got)
	}
}

func TestResolveInsideLibrary(t *testing.T) {
	ix := NewIndex()
	lib, _ := newJDK()
	ix.SetLibrary(lib)
	const path = "java.base/java/lang/String.java"
	src := jdk[path]
	resolve := func(needle string, nth int) (Location, bool) {
		l, ok := ix.ResolveLibrary(path, []byte(src), offsetOf(t, src, needle, nth, 0))
		l.rule = ""
		return l, ok
	}
	// A type of the library's own package, a local, and a declaration.
	got, ok := resolve("CharSequence", 0)
	want := declAtSrc(t, "java.base/java/lang/CharSequence.java", jdk["java.base/java/lang/CharSequence.java"], "CharSequence {")
	want.Library, want.Name = true, "CharSequence"
	if !ok || got != want {
		t.Errorf("CharSequence resolved to %+v, want %+v", got, want)
	}
	got, ok = resolve("n;", 0)
	want = declAtSrc(t, path, src, "n = ")
	want.Library, want.Name, want.Local = true, "n", true
	if !ok || got != want {
		t.Errorf("local n resolved to %+v, want %+v", got, want)
	}
	got, ok = resolve("length()", 0)
	if !ok || !got.Self || !got.Library || got.Path != path {
		t.Errorf("declaration length resolved to %+v", got)
	}
	// The library source is not added to the index as a working-tree file.
	if ix.Has(path) || ix.Len() != 0 {
		t.Errorf("a library source entered the index: %v", ix.Paths())
	}
}

func TestUsagesOfLibraryDeclarations(t *testing.T) {
	ix := NewIndex()
	lib, _ := newJDK()
	ix.SetLibrary(lib)
	ix.Put(ParseFile(useJDKPath, []byte(useJDK)))
	const path = "java.base/java/lang/String.java"
	src := jdk[path]

	// String.length() used in the working tree: the call on a String, not
	// Collection.size() and not the array's length inside String itself.
	decl, ok := ix.ResolveLibrary(path, []byte(src), offsetOf(t, src, "length()", 0, 0))
	if !ok || !decl.Self {
		t.Fatalf("length: %+v ok=%v", decl, ok)
	}
	got := ix.Usages(useJDKPath, []byte(useJDK), decl, ix.UsageNames(decl, decl.Name))
	want := declAtSrc(t, useJDKPath, useJDK, "length()")
	if len(got) != 1 || got[0].Path != useJDKPath || got[0].Library || got[0].Line != want.Line || got[0].Column != want.Column {
		t.Errorf("usages of String.length = %+v, want one at %+v", got, want)
	}

	// A local declared in a library source is used only there.
	decl, ok = ix.ResolveLibrary(path, []byte(src), offsetOf(t, src, "n = ", 0, 0))
	if !ok || !decl.Self || !decl.Local {
		t.Fatalf("n: %+v ok=%v", decl, ok)
	}
	got = ix.LibraryUsages(path, []byte(src), decl, []string{"n"})
	want = declAtSrc(t, path, src, "n;")
	if len(got) != 1 || !got[0].Library || got[0].Line != want.Line || got[0].Column != want.Column {
		t.Errorf("usages of n = %+v, want one at %+v", got, want)
	}
	// The same path read as a working-tree file is another file: the
	// declaration there is not this one.
	if got := ix.Usages(path, []byte(src), decl, []string{"n"}); len(got) != 0 {
		t.Errorf("working-tree namesake listed library usages: %+v", got)
	}
}

func TestLibraryReadFailure(t *testing.T) {
	ix := NewIndex()
	boom := errors.New("archive gone")
	calls := 0
	lib := NewLibrary([]string{"java.base/java/lang/Object.java"}, func(string) ([]byte, error) {
		calls++
		return nil, boom
	})
	ix.SetLibrary(lib)
	src := "package a;\n\nclass A { Object o; Object p; }\n"
	ix.Put(ParseFile("a/A.java", []byte(src)))
	for range 2 {
		if got, ok := ix.Resolve("a/A.java", []byte(src), offsetOf(t, src, "Object", 0, 0)); ok {
			t.Fatalf("resolved %+v from an unreadable library", got)
		}
	}
	if calls != 1 {
		t.Errorf("unreadable source read %d times, want once", calls)
	}
	if err := lib.Err(); !errors.Is(err, boom) || !strings.Contains(err.Error(), "Object.java") {
		t.Errorf("Err() = %v", err)
	}
	if err := lib.Err(); err != nil {
		t.Errorf("Err() reported twice: %v", err)
	}
}

// TestLibraryConcurrentLookups runs under -race in CI: usage searches resolve
// on several goroutines, all loading the same library sources.
func TestLibraryConcurrentLookups(t *testing.T) {
	ix := NewIndex()
	lib, read := newJDK()
	ix.SetLibrary(lib)
	ix.Put(ParseFile(useJDKPath, []byte(useJDK)))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, needle := range []string{"trim()", "stream()", "hashCode()", "getKey()"} {
				if _, ok := ix.Resolve(useJDKPath, []byte(useJDK), offsetOf(t, useJDK, needle, 0, 0)); !ok {
					t.Errorf("%s not resolved", needle)
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range read() {
		if seen[p] {
			t.Errorf("%s read twice", p)
		}
		seen[p] = true
	}
}
