package codenav

// This file is a Lithe addition: cases found by comparing against JDT LS on
// Nacos where this index answered differently or not at all.

import (
	"strings"
	"testing"
)

var overloadProject = map[string]string{
	"src/o/Error.java": `package o;

public class Error {
    public Error() {}
    public Error(int code) {}
    public Error(String msg) {}
    public Error(Error cause) {}
}
`,
	"src/o/Cause.java": `package o;

public class Cause extends Error {}
`,
	"src/o/Cache.java": `package o;

import java.util.Set;

public class Cache {
    public void init(String packageName) {}
    public void init(Set<Class<?>> classes) {}
    public void init(long ttl, boolean lazy) {}
    public void init(Object key, boolean lazy) {}
    public void put(char c) {}
    public void put(double d) {}
    public void put(Cache other) {}
    public static <T> Cache builder() { return null; }
    public static void check(boolean ok, Object message) {}
    public static void check(boolean ok, String template, Object... args) {}
    public Cache self() { return this; }
}
`,
	"src/o/Use.java": `package o;

import java.util.HashSet;
import java.util.Set;

class Use {
    void run(Set<Class<?>> classes, Cache cache) {
        Error a = new Error("user not found!");
        Error b = new Error(404);
        Error c = new Error(new Cause());
        Error d = new Error("a" + 1);
        cache.init(classes);
        cache.init("com.acme");
        cache.init(new HashSet<Class<?>>());
        cache.init(5L, true);
        cache.init(cache, false);
        cache.put('x');
        cache.put(1.5);
        cache.put(-2);
        cache.put(cache);
        cache.put((Cache) null);
        Cache.<String>builder().self();
        Cache.check(classes.isEmpty(), "no classes");
        Cache.check(true, "%s missing", "x");
    }
}
`,
}

func TestOverloadsWithTheSameArity(t *testing.T) {
	ix := NewIndex()
	for p, src := range overloadProject {
		ix.Put(ParseFile(p, []byte(src)))
	}
	const use = "src/o/Use.java"
	at := func(path, needle string) Location {
		src := overloadProject[path]
		off := strings.Index(src, needle)
		if off < 0 {
			t.Fatalf("%q not found in %s", needle, path)
		}
		return Location{Path: path, Line: strings.Count(src[:off], "\n"), Column: off - (strings.LastIndexByte(src[:off], '\n') + 1)}
	}
	const errT, cache = "src/o/Error.java", "src/o/Cache.java"
	cases := []struct {
		name, click string
		skip        int
		want        Location
	}{
		{"string literal picks the String constructor", `Error("user`, 0, at(errT, "Error(String msg)")},
		{"int literal picks the int constructor", `Error(404)`, 0, at(errT, "Error(int code)")},
		{"subtype argument picks the supertype parameter", `Error(new Cause())`, 0, at(errT, "Error(Error cause)")},
		{"string concatenation is a String", `Error("a" + 1)`, 0, at(errT, "Error(String msg)")},
		{"typed parameter picks the matching overload", `init(classes)`, 0, at(cache, "init(Set")},
		{"string literal picks String over Set", `init("com`, 0, at(cache, "init(String")},
		{"new expression type", `init(new HashSet`, 0, at(cache, "init(Set")},
		{"long literal", `init(5L`, 0, at(cache, "init(long")},
		{"reference argument goes to Object", `init(cache, false)`, 0, at(cache, "init(Object")},
		{"char literal", `put('x')`, 0, at(cache, "put(char")},
		{"double literal", `put(1.5)`, 0, at(cache, "put(double")},
		{"negative int widens to double, not char", `put(-2)`, 0, at(cache, "put(double")},
		{"project type", `put(cache)`, 0, at(cache, "put(Cache")},
		{"cast", `put((Cache)`, 0, at(cache, "put(Cache")},
		{"explicit type arguments", `builder()`, 0, at(cache, "builder()")},
		{"a fixed-arity overload beats varargs", `check(classes`, 0, at(cache, "check(boolean ok, Object")},
		{"varargs when the fixed one cannot take the arguments", `check(true`, 0, at(cache, "check(boolean ok, String")},
		{"call after explicit type arguments", `self();`, 0, at(cache, "self()")},
	}
	src := []byte(overloadProject[use])
	for _, c := range cases {
		off := strings.Index(string(src), c.click)
		if off < 0 {
			t.Fatalf("%s: %q not found", c.name, c.click)
		}
		got, ok := ix.Resolve(use, src, off+c.skip)
		if !ok {
			t.Errorf("%s: no declaration, want %v", c.name, c.want)
			continue
		}
		if got.Path != c.want.Path || got.Line != c.want.Line || got.Column != c.want.Column {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

var jdtlsCaseProject = map[string]string{
	"src/q/Manager.java": `package q;

public class Manager {
    public @interface Extractor {
        Class<?> rpc();
    }
}
`,
	"src/q/Handler.java": `package q;

class Handler {
    @Manager.Extractor(rpc = Handler.class)
    void handle() {}

    static void check(Class<?> raw, final Type... typeArguments) {
        if (typeArguments.length != 0) {
            return;
        }
    }

    byte[] key(String... keys) { return keys[0].getBytes(); }
}
`,
}

func TestJDTLSCases(t *testing.T) {
	ix := NewIndex()
	for p, src := range jdtlsCaseProject {
		ix.Put(ParseFile(p, []byte(src)))
	}
	const h = "src/q/Handler.java"
	at := func(path, needle string, nth int) Location {
		src := jdtlsCaseProject[path]
		off := offsetOf(t, src, needle, nth, 0)
		return Location{Path: path, Line: strings.Count(src[:off], "\n"), Column: off - (strings.LastIndexByte(src[:off], '\n') + 1)}
	}
	cases := []struct {
		name, click string
		nth         int
		want        Location
	}{
		{"qualified annotation", "Extractor(", 0, at("src/q/Manager.java", "Extractor {", 0)},
		{"attribute of a qualified annotation", "rpc =", 0, at("src/q/Manager.java", "rpc()", 0)},
		{"varargs parameter declaration", "typeArguments)", 0, at(h, "typeArguments)", 0)},
		{"varargs parameter use", "typeArguments.length", 0, at(h, "typeArguments)", 0)},
		{"varargs parameter after a plain type", "keys)", 0, at(h, "keys)", 0)},
		{"varargs parameter indexed", "keys[0]", 0, at(h, "keys)", 0)},
	}
	src := []byte(jdtlsCaseProject[h])
	for _, c := range cases {
		off := offsetOf(t, string(src), c.click, c.nth, 0)
		got, ok := ix.Resolve(h, src, off)
		if !ok {
			t.Errorf("%s: no declaration, want %v", c.name, c.want)
			continue
		}
		if got.Path != c.want.Path || got.Line != c.want.Line || got.Column != c.want.Column {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
