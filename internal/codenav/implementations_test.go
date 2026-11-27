package codenav

import (
	"bytes"
	"strings"
	"testing"
)

func buildNavIndex(files map[string]string) *Index {
	ix := NewIndex()
	for p, src := range files {
		ix.Put(ParseFile(p, []byte(src)))
	}
	return ix
}

func indexOf(src, needle string) int {
	i := strings.Index(src, needle)
	if i < 0 {
		panic("missing needle: " + needle)
	}
	return i
}

func TestImplementationsAndSuperDeclarationsAcrossHierarchy(t *testing.T) {
	greeterSrc := `package demo;

public interface Greeter {
    String greet(String name);
    String greet(int times);
    private void secret() {}
    static void helper() {}
}
`
	baseSrc := `package demo;

public abstract class BaseGreeter implements Greeter {
    public String greet(String name) { return "Hi " + name; }
    public String greet(int times) { return "x"; }
}
`
	customSrc := `package demo;

public class CustomGreeter extends BaseGreeter {
    public String greet(String name) { return "Custom " + name; }
}
`
	finalSrc := `package demo;

public final class ClosedGreeter extends CustomGreeter {
    public String greet(String name) { return "Final"; }
}
`
	callerSrc := `package demo;

class Caller {
    void run(Greeter g) {
        g.greet("world");
    }
}
`
	ix := buildNavIndex(map[string]string{
		"demo/Greeter.java":       greeterSrc,
		"demo/BaseGreeter.java":   baseSrc,
		"demo/CustomGreeter.java": customSrc,
		"demo/ClosedGreeter.java": finalSrc,
		"demo/Caller.java":        callerSrc,
	})

	// 1. Type implementations from Greeter include direct + transitive subclasses.
	impls := ix.Implementations("demo/Greeter.java", []byte(greeterSrc), indexOf(greeterSrc, "Greeter {"))
	if len(impls) != 3 {
		t.Fatalf("Greeter implementations = %+v, want 3 subtypes", impls)
	}

	// 2. Method implementations on greet(String) differentiate overload greet(int).
	methodImpls := ix.Implementations("demo/Greeter.java", []byte(greeterSrc), indexOf(greeterSrc, "greet(String name)"))
	if len(methodImpls) != 3 {
		t.Fatalf("greet(String) implementations = %+v, want 3 overrides", methodImpls)
	}
	intImpls := ix.Implementations("demo/Greeter.java", []byte(greeterSrc), indexOf(greeterSrc, "greet(int times)"))
	if len(intImpls) != 1 || intImpls[0].Path != "demo/BaseGreeter.java" {
		t.Fatalf("greet(int) implementations = %+v, want only BaseGreeter", intImpls)
	}

	// 3. Call-site implementation jump works directly without landing on declaration first.
	callImpls := ix.Implementations("demo/Caller.java", []byte(callerSrc), indexOf(callerSrc, `greet("world")`))
	if len(callImpls) != 3 {
		t.Fatalf("call-site greet implementations = %+v, want 3 overrides", callImpls)
	}

	// 4. Final class has zero implementations; private/static interface methods have zero implementations.
	closedImpls := ix.Implementations("demo/ClosedGreeter.java", []byte(finalSrc), indexOf(finalSrc, "ClosedGreeter"))
	if len(closedImpls) != 0 {
		t.Fatalf("final class implementations = %+v, want 0", closedImpls)
	}
	if got := ix.Implementations("demo/Greeter.java", []byte(greeterSrc), indexOf(greeterSrc, "secret()")); len(got) != 0 {
		t.Fatalf("private method implementations = %+v, want 0", got)
	}
	if got := ix.Implementations("demo/Greeter.java", []byte(greeterSrc), indexOf(greeterSrc, "helper()")); len(got) != 0 {
		t.Fatalf("static method implementations = %+v, want 0", got)
	}

	// 5. Super declarations from ClosedGreeter.greet(String) find CustomGreeter, BaseGreeter, and Greeter.
	supers := ix.SuperDeclarations("demo/ClosedGreeter.java", []byte(finalSrc), indexOf(finalSrc, "greet(String name)"))
	if len(supers) != 3 {
		t.Fatalf("ClosedGreeter.greet supers = %+v, want 3 ancestors", supers)
	}

	// 6. Type super declarations from CustomGreeter find BaseGreeter.
	typeSupers := ix.SuperDeclarations("demo/CustomGreeter.java", []byte(customSrc), indexOf(customSrc, "CustomGreeter"))
	if len(typeSupers) != 1 || typeSupers[0].Path != "demo/BaseGreeter.java" {
		t.Fatalf("CustomGreeter supers = %+v, want BaseGreeter", typeSupers)
	}

	// 7. Navigation markers on intermediate CustomGreeter emit BOTH up and down icons.
	markers := ix.NavigationMarkers("demo/CustomGreeter.java", []byte(customSrc))
	if len(markers) != 4 {
		t.Fatalf("CustomGreeter markers = %+v, want 4 (class up/down + greet up/down)", markers)
	}
	// CustomGreeter implements Greeter indirectly, so method up prefers "interface" because ancestor is Greeter.
	for _, m := range markers {
		if m.Line == 3 && m.Direction == "up" && m.Relation != "interface" {
			t.Errorf("CustomGreeter.greet up relation = %q, want interface", m.Relation)
		}
		if m.Line == 2 && m.Direction == "up" && m.Relation != "inheritance" {
			t.Errorf("CustomGreeter class up relation = %q, want inheritance", m.Relation)
		}
	}
}

func TestArraySignatureImplementations(t *testing.T) {
	const base = `package demo;

interface Buffer<T> {
    void write(String text);
    void write(String... texts);
    void put(T[] values);
}
`
	const sub = `package demo;

class TextBuffer implements Buffer<String> {
    public void write(String text) {}
    public void write(String[] texts) {}
    public void put(String[] values) {}
}
`
	const basePath, subPath = "demo/Buffer.java", "demo/TextBuffer.java"
	ix := buildNavIndex(map[string]string{basePath: base, subPath: sub})
	for _, tt := range []struct{ baseMethod, subMethod string }{
		{"write(String text)", "write(String text)"},
		{"write(String... texts)", "write(String[] texts)"},
		{"put(T[] values)", "put(String[] values)"},
	} {
		t.Run(tt.baseMethod, func(t *testing.T) {
			got := ix.Implementations(basePath, []byte(base), indexOf(base, tt.baseMethod))
			want := declAtSrc(t, subPath, sub, tt.subMethod)
			if len(got) != 1 || got[0].Path != want.Path || got[0].Line != want.Line || got[0].Column != want.Column {
				t.Fatalf("implementations = %+v, want only %+v", got, want)
			}
		})
	}
}

func TestCyclicInheritanceGuardDoesNotLoop(t *testing.T) {
	aSrc := "package cyc; class A extends B { void m() {} }\n"
	bSrc := "package cyc; class B extends A { void m() {} }\n"
	ix := buildNavIndex(map[string]string{
		"cyc/A.java": aSrc,
		"cyc/B.java": bSrc,
	})

	impls := ix.Implementations("cyc/A.java", []byte(aSrc), bytes.Index([]byte(aSrc), []byte("A extends")))
	if len(impls) != 1 || impls[0].Path != "cyc/B.java" {
		t.Fatalf("cyclic implementations = %+v, want only B", impls)
	}
	supers := ix.SuperDeclarations("cyc/A.java", []byte(aSrc), bytes.Index([]byte(aSrc), []byte("m()")))
	if len(supers) != 1 || supers[0].Path != "cyc/B.java" {
		t.Fatalf("cyclic supers = %+v, want only B.m", supers)
	}
	markers := ix.NavigationMarkers("cyc/A.java", []byte(aSrc))
	if len(markers) != 4 {
		t.Fatalf("cyclic markers = %+v, want 4", markers)
	}
}
