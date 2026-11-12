package codenav

import (
	"strings"
	"testing"
)

// A small multi-package project that exercises each resolution rule. The
// sources compile against Lombok, slf4j and the JDK; those stay outside the
// index, so names from them must resolve to nothing.
var project = map[string]string{
	"src/com/acme/model/BaseEntity.java": `package com.acme.model;

public abstract class BaseEntity {
    protected Long id;
    public Long getId() { return id; }
}
`,
	"src/com/acme/model/Named.java": `package com.acme.model;

public interface Named {
    String displayName();
}
`,
	"src/com/acme/model/Status.java": `package com.acme.model;

public enum Status {
    ACTIVE, DISABLED;
}
`,
	"src/com/acme/model/User.java": `package com.acme.model;

import java.util.HashMap;
import java.util.Map;
import lombok.Builder;
import lombok.Data;

@Data
@Builder
public class User extends BaseEntity implements Named {
    public static final int MAX = 10;
    private String name;
    private Address address;
    private final Map<String, Integer> scores = new HashMap<String, Integer>(), extra = null;

    public User(String name) { this.name = name; }

    public String greet(String who) { return "hi " + who; }

    public String greet(String who, int times) { return who; }

    public String displayName() { return name; }

    public <T> T pick(T value) { return value; }

    public static class Address {
        public String city;
        public Address copy() { return this; }
    }
}
`,
	"src/com/acme/service/UserRepo.java": `package com.acme.service;

class UserRepo {
    java.util.List<com.acme.model.User> findAll() { return null; }
    com.acme.model.User first() { return null; }
}
`,
	"src/com/acme/util/Strings.java": `package com.acme.util;

public final class Strings {
    public static String trim(String s) { return s; }
}
`,
	"src/com/acme/web/Secured.java": `package com.acme.web;

public @interface Secured {
    String action() default "";
}
`,
	"src/com/acme/service/UserService.java": `package com.acme.service;

import com.acme.model.*;
import com.acme.web.Secured;
import io.swagger.v3.oas.annotations.media.Schema;
import java.util.List;
import lombok.extern.slf4j.Slf4j;

import static com.acme.util.Strings.trim;

@Slf4j
public class UserService {
    private final UserRepo repo = new UserRepo();

    @Secured(action = "read")
    @Schema(description = "run")
    public String run(User user, Status status) {
        String local = user.greet("a");
        log.info(local);
        User copy = new User("x");
        copy.setName("y").setAddress(null);
        long n = user.getId();
        List<User> users = repo.findAll();
        users.forEach(u -> u.greet("b", 2));
        String twice = user.greet("c", 2);
        switch (status) {
            case ACTIVE:
                break;
            default:
                break;
        }
        User.Address a = new User.Address();
        String c = a.copy().city;
        String built = User.builder().name("z").build().greet("q");
        String first = repo.first().displayName();
        return trim(local) + helper() + User.MAX + c + built + first + n;
    }

    private String helper() { return ""; }
}
`,
}

func newProject(t *testing.T) *Index {
	t.Helper()
	ix := NewIndex()
	for p, src := range project {
		ix.Put(ParseFile(p, []byte(src)))
	}
	if ix.Len() != len(project) {
		t.Fatalf("Len = %d, want %d", ix.Len(), len(project))
	}
	return ix
}

// offsetOf returns the byte offset of the nth (0-based) occurrence of needle
// in src, plus skip bytes.
func offsetOf(t *testing.T, src, needle string, nth, skip int) int {
	t.Helper()
	off := -1
	for i := 0; i <= nth; i++ {
		k := strings.Index(src[off+1:], needle)
		if k < 0 {
			t.Fatalf("occurrence %d of %q not found", nth, needle)
		}
		off += 1 + k
	}
	return off + skip
}

// declAt is the location of the nth occurrence of needle (its first byte plus
// skip) in project file path.
func declAt(t *testing.T, path, needle string, nth, skip int) Location {
	t.Helper()
	src := project[path]
	off := offsetOf(t, src, needle, nth, skip)
	line := strings.Count(src[:off], "\n")
	col := off - (strings.LastIndexByte(src[:off], '\n') + 1)
	return Location{Path: path, Line: line, Column: col}
}

func TestResolveRules(t *testing.T) {
	const svc = "src/com/acme/service/UserService.java"
	const user = "src/com/acme/model/User.java"
	cases := []struct {
		name  string
		click string // the occurrence to click in UserService.java
		nth   int
		skip  int
		want  *Location // nil: no declaration in the index
	}{
		{"wildcard import type", "User user", 0, 0, ptr(declAt(t, user, "User extends", 0, 0))},
		{"same-package type without import", "UserRepo repo", 0, 0, ptr(declAt(t, "src/com/acme/service/UserRepo.java", "UserRepo", 0, 0))},
		{"parameter", "user.greet", 0, 0, ptr(declAt(t, svc, "user, Status", 0, 0))},
		{"overload by arity: one argument", "greet(\"a\")", 0, 0, ptr(declAt(t, user, "greet(String who)", 0, 0))},
		{"overload by arity: two arguments", "greet(\"c\", 2)", 0, 0, ptr(declAt(t, user, "greet(String who, int", 0, 0))},
		{"lambda parameter", "u.greet", 0, 0, ptr(declAt(t, svc, "u -> ", 0, 0))},
		{"call on an untyped lambda parameter (needs JDK generics)", "greet(\"b\", 2)", 0, 0, nil},
		{"local variable", "trim(local)", 0, 5, ptr(declAt(t, svc, "local = ", 0, 0))},
		{"static import", "trim(local)", 0, 0, ptr(declAt(t, "src/com/acme/util/Strings.java", "trim(", 0, 0))},
		{"private method of the enclosing class", "helper()", 0, 0, ptr(declAt(t, svc, "helper()", 1, 0))},
		{"inherited method", "getId()", 0, 0, ptr(declAt(t, "src/com/acme/model/BaseEntity.java", "getId", 0, 0))},
		{"constructor by arity", "User(\"x\")", 0, 0, ptr(declAt(t, user, "User(String name)", 0, 0))},
		{"Lombok setter", "setName", 0, 0, ptr(declAt(t, user, "name;", 0, 0))},
		{"Lombok chained setter", "setAddress", 0, 0, ptr(declAt(t, user, "address;", 0, 0))},
		{"Lombok logger", "log.info", 0, 0, ptr(declAt(t, svc, "Slf4j\n", 0, 0))},
		{"Lombok builder", "builder()", 0, 0, ptr(declAt(t, user, "Builder\n", 0, 0))},
		{"Lombok builder setter", "name(\"z\")", 0, 0, ptr(declAt(t, user, "Builder\n", 0, 0))},
		{"call after build()", "greet(\"q\")", 0, 0, ptr(declAt(t, user, "greet(String who)", 0, 0))},
		{"enum constant in a case label", "ACTIVE:", 0, 0, ptr(declAt(t, "src/com/acme/model/Status.java", "ACTIVE", 0, 0))},
		{"nested type", "Address()", 0, 0, ptr(declAt(t, user, "Address {", 0, 0))},
		{"field through a call chain", "city;", 0, 0, ptr(declAt(t, user, "city;", 0, 0))},
		{"interface method through a project return type", "displayName()", 0, 0, ptr(declAt(t, user, "displayName()", 0, 0))},
		{"static constant", "MAX +", 0, 0, ptr(declAt(t, user, "MAX", 0, 0))},
		{"field of the enclosing class", "repo.findAll", 0, 0, ptr(declAt(t, svc, "repo = ", 0, 0))},
		{"annotation type", "Secured(", 0, 0, ptr(declAt(t, "src/com/acme/web/Secured.java", "Secured", 0, 0))},
		{"annotation attribute", "action =", 0, 0, ptr(declAt(t, "src/com/acme/web/Secured.java", "action()", 0, 0))},
		{"declaration itself", "run(", 0, 0, ptr(declAt(t, svc, "run(", 0, 0))},
		{"library annotation attribute", "description =", 0, 0, nil},
		{"JDK type", "String local", 0, 0, nil},
		{"JDK method on a JDK type", "forEach", 0, 0, nil},
		{"keyword", "return trim", 0, 0, nil},
		{"string literal", "\"read\"", 0, 1, nil},
		{"comment-free whitespace", "    private final", 0, 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ix := newProject(t)
			src := project[svc]
			off := offsetOf(t, src, c.click, c.nth, c.skip)
			got, ok := ix.Resolve(svc, []byte(src), off)
			if c.want == nil {
				if ok {
					t.Fatalf("resolved to %s:%d:%d (%s), want nothing", got.Path, got.Line+1, got.Column+1, got.rule)
				}
				return
			}
			if !ok {
				t.Fatalf("not resolved, want %s:%d:%d", c.want.Path, c.want.Line+1, c.want.Column+1)
			}
			if got.Path != c.want.Path || got.Line != c.want.Line || got.Column != c.want.Column {
				t.Fatalf("resolved to %s:%d:%d (%s), want %s:%d:%d", got.Path, got.Line+1, got.Column+1, got.rule,
					c.want.Path, c.want.Line+1, c.want.Column+1)
			}
		})
	}
}

func ptr(l Location) *Location { return &l }

func TestTypeParameter(t *testing.T) {
	const user = "src/com/acme/model/User.java"
	ix := newProject(t)
	src := project[user]
	off := offsetOf(t, src, "T pick", 0, 0)
	got, ok := ix.Resolve(user, []byte(src), off)
	want := declAt(t, user, "T> T pick", 0, 0)
	if !ok || got.Line != want.Line || got.Column != want.Column {
		t.Fatalf("type parameter resolved to %+v (%v), want %+v", got, ok, want)
	}
}

// Regressions found while measuring against JDTLS (ADR 0040).
func TestParserRegressions(t *testing.T) {
	f := ParseFile("User.java", []byte(project["src/com/acme/model/User.java"]))
	if len(f.Types) != 1 {
		t.Fatalf("types = %d", len(f.Types))
	}
	u := f.Types[0]
	// Class annotations after the imports belong to the class, not the header.
	if u.Lombok&lombokGetter == 0 || u.Lombok&lombokSetter == 0 || u.BuilderAt.Line < 0 {
		t.Errorf("Lombok annotations lost: lombok=%b builder=%v", u.Lombok, u.BuilderAt)
	}
	// "new HashMap<String, Integer>()" must not declare a field named Integer;
	// the real second declarator (extra) must still be found.
	var fields []string
	for _, m := range u.Members {
		if m.Kind == kField {
			fields = append(fields, m.Name)
		}
	}
	if got := strings.Join(fields, ","); got != "MAX,name,address,scores,extra" {
		t.Errorf("fields = %s", got)
	}
	if len(u.Supers) != 2 || u.Supers[0] != "BaseEntity" || u.Supers[1] != "Named" {
		t.Errorf("supers = %v", u.Supers)
	}
}

func TestIncrementalUpdate(t *testing.T) {
	const svc = "src/com/acme/service/UserService.java"
	const base = "src/com/acme/model/BaseEntity.java"
	ix := newProject(t)
	src := project[svc]
	off := offsetOf(t, src, "getId()", 0, 0)

	moved := "package com.acme.model;\n\npublic abstract class BaseEntity {\n    protected Long id;\n\n\n    public Long getId() { return id; }\n}\n"
	ix.Put(ParseFile(base, []byte(moved)))
	got, ok := ix.Resolve(svc, []byte(src), off)
	if !ok || got.Path != base || got.Line != 6 {
		t.Fatalf("after Put: %+v %v, want %s:7", got, ok, base)
	}
	ix.Remove(base)
	if ix.Has(base) {
		t.Fatal("Remove kept the file")
	}
	if got, ok := ix.Resolve(svc, []byte(src), off); ok {
		t.Fatalf("after Remove: resolved to %+v", got)
	}
	// Resolve indexes the file it is given: an edited service sees its own
	// new declarations without a separate Put.
	edited := strings.Replace(src, "private String helper()", "\n\nprivate String helper()", 1)
	got, ok = ix.Resolve(svc, []byte(edited), offsetOf(t, edited, "helper()", 0, 0))
	want := strings.Count(edited[:offsetOf(t, edited, "helper()", 1, 0)], "\n")
	if !ok || got.Line != want {
		t.Fatalf("edited file: %+v %v, want line %d", got, ok, want+1)
	}
}

// Columns are byte offsets within the line, whatever the line endings and
// whatever precedes the name on the line.
func TestColumnsWithCRLFAndUnicode(t *testing.T) {
	src := "package p;\r\n\r\nclass Grüße {\r\n\tint größe;\r\n\tint get() { return größe; }\r\n}\r\n"
	ix := NewIndex()
	off := offsetOf(t, src, "größe; }", 0, 0)
	got, ok := ix.Resolve("p/Grüße.java", []byte(src), off)
	want := offsetOf(t, src, "größe;", 0, 0) - offsetOf(t, src, "\tint größe", 0, 0)
	if !ok || got.Line != 3 || got.Column != want {
		t.Fatalf("got %+v %v, want line 4 column %d", got, ok, want)
	}
}

// Malformed input (every truncation of a real file) must never panic and
// never resolve outside the source it was given.
func TestTruncatedSourcesDoNotPanic(t *testing.T) {
	const svc = "src/com/acme/service/UserService.java"
	ix := newProject(t)
	full := project[svc]
	for n := 0; n <= len(full); n += 3 {
		src := []byte(full[:n])
		for off := 0; off < n; off += 11 {
			if l, ok := ix.Resolve(svc, src, off); ok && l.Path == svc && l.Line > strings.Count(full[:n], "\n") {
				t.Fatalf("prefix %d offset %d: line %d is past the end", n, off, l.Line)
			}
		}
	}
}

func TestSupports(t *testing.T) {
	for p, want := range map[string]bool{"a/B.java": true, "B.JAVA": true, "b.kt": false, "java": false, "x.java.txt": false} {
		if Supports(p) != want {
			t.Errorf("Supports(%q) = %v", p, !want)
		}
	}
}

func BenchmarkResolve(b *testing.B) {
	const svc = "src/com/acme/service/UserService.java"
	ix := NewIndex()
	for p, src := range project {
		ix.Put(ParseFile(p, []byte(src)))
	}
	src := []byte(project[svc])
	off := strings.Index(project[svc], "greet(\"q\")")
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := ix.Resolve(svc, src, off); !ok {
			b.Fatal("not resolved")
		}
	}
}

// A member type brought in by a static import (Hadoop marks its API with
// "import static ...InterfaceStability.*;" and "@Evolving").
func TestStaticImportOfMemberType(t *testing.T) {
	ix := NewIndex()
	ix.Put(ParseFile("a/Stability.java", []byte("package a;\n\npublic class Stability {\n    public @interface Evolving {}\n    public @interface Stable {}\n}\n")))
	src := "package b;\n\nimport static a.Stability.*;\nimport static a.Stability.Stable;\n\n@Evolving\n@Stable\npublic class Use {}\n"
	ix.Put(ParseFile("b/Use.java", []byte(src)))
	for _, c := range []struct{ click, want string }{{"Evolving\n", "Evolving {}"}, {"Stable\npublic", "Stable {}"}} {
		got, ok := ix.Resolve("b/Use.java", []byte(src), offsetOf(t, src, c.click, 0, 0))
		want := declAtSrc(t, "a/Stability.java", "package a;\n\npublic class Stability {\n    public @interface Evolving {}\n    public @interface Stable {}\n}\n", c.want)
		if !ok || got.Path != want.Path || got.Line != want.Line || got.Column != want.Column {
			t.Errorf("%q resolved to %+v ok=%v, want %+v", c.click, got, ok, want)
		}
	}
}

func declAtSrc(t *testing.T, path, src, needle string) Location {
	t.Helper()
	off := offsetOf(t, src, needle, 0, 0)
	return Location{Path: path, Line: strings.Count(src[:off], "\n"), Column: off - (strings.LastIndexByte(src[:off], '\n') + 1)}
}
