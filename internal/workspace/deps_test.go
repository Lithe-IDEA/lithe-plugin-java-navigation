package workspace

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mavenRepo is a local repository holding the deps package's fixture jars:
// com.example:lib:1.0 (class files only) and com.example:helper:2.0 (with a
// sources jar).
func mavenRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fixture := filepath.Join("..", "deps", "testdata", "fixture")
	for _, a := range []struct{ artifact, version, file string }{
		{"lib", "1.0", "lib-1.0.jar"},
		{"helper", "2.0", "helper-2.0.jar"},
		{"helper", "2.0", "helper-2.0-sources.jar"},
	} {
		dir := filepath.Join(root, "com", "example", a.artifact, a.version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		copyFile(t, filepath.Join(fixture, a.file), filepath.Join(dir, a.file))
		pom := "<project><groupId>com.example</groupId><artifactId>" + a.artifact + "</artifactId><version>" + a.version + "</version></project>"
		if err := os.WriteFile(filepath.Join(dir, a.artifact+"-"+a.version+".pom"), []byte(pom), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// treeHash hashes every file under root, to show a run left it unchanged.
func treeHash(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		io.WriteString(h, p+"\x00")
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

const depsPOM = `<project><groupId>com.acme</groupId><artifactId>app</artifactId><version>1</version>
<dependencies>
  <dependency><groupId>com.example</groupId><artifactId>lib</artifactId><version>1.0</version></dependency>
</dependencies></project>`

const useDeps = `package com.acme;

import com.example.lib.Container;
import com.example.lib.Schema;

public class UseDeps {
    @Schema(description = "a box", requiredMode = Schema.RequiredMode.REQUIRED)
    private Container<String> box = new Container.Builder().add("x").build();

    int size() {
        return box.size();
    }
}
`

// Names from a Maven dependency open a stub of its class files, and
// navigation continues inside the stub; the local repository is never
// written.
func TestDefinitionIntoADependencyJarAndBack(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"pom.xml": depsPOM, "src/main/java/com/acme/UseDeps.java": useDeps})
	repo := mavenRepo(t)
	before := treeHash(t, repo)
	w := New(Options{Root: root, State: filepath.Join(t.TempDir(), "state"), Dependencies: true, MavenRepo: repo,
		ListFiles: walkFiles, Freshness: -1, Logf: t.Logf})
	doc := w.DocOf(filepath.Join(root, "src/main/java/com/acme/UseDeps.java"))

	def := func(d Doc, text, needle string, n, skip int) Target {
		t.Helper()
		tgt, ok, err := w.Definition(ctx(t), d, []byte(text), at(t, text, needle, n, skip))
		if err != nil || !ok {
			t.Fatalf("%q: %v %v", needle, ok, err)
		}
		return tgt
	}
	line := func(tgt Target) string {
		t.Helper()
		b, err := os.ReadFile(tgt.File)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(strings.Split(string(b), "\n")[tgt.Line])
	}
	for _, c := range []struct {
		needle string
		n      int
		want   string
	}{
		{"Schema(", 0, "public @interface Schema {"},
		{"description =", 0, `String description() default "";`},
		{"RequiredMode.REQUIRED", 0, "public enum RequiredMode {"},
		{"REQUIRED)", 0, "REQUIRED,"},
		{"Container<String>", 0, "public class Container<E> extends AbstractList<E> implements Serializable {"},
		{"Builder()", 0, "public Builder() { /* compiled code */ }"},
		{"add(", 0, "public Container.Builder add(Object o) { /* compiled code */ }"},
		{"build()", 0, "public Container<Object> build() { /* compiled code */ }"},
		{"size();", 0, "public int size() { /* compiled code */ }"},
	} {
		tgt := def(doc, useDeps, c.needle, c.n, 1)
		if !strings.Contains(filepath.ToSlash(tgt.File), "/state/dependency-sources/com.example_lib_1.0-") {
			t.Errorf("%q opened %s", c.needle, tgt.File)
		}
		if got := line(tgt); got != c.want {
			t.Errorf("%q landed on %q, want %q", c.needle, got, c.want)
		}
	}

	// The stub is a library source: navigation continues inside it, into
	// another class of the jar and into the JDK-less java.util it names
	// (which resolves to nothing without a JDK).
	stub := def(doc, useDeps, "Container<String>", 0, 1)
	sdoc := w.DocOf(stub.File)
	if sdoc.Library == "" || sdoc.Rel != "" {
		t.Fatalf("DocOf(stub) = %+v", sdoc)
	}
	text, _ := os.ReadFile(stub.File)
	inner := def(sdoc, string(text), "Container.Builder add", 0, 10)
	if got := line(inner); got != "public static class Builder {" {
		t.Errorf("Builder from inside the stub landed on %q", got)
	}
	// Usages of a dependency's declaration are the working tree's.
	refs, _, err := w.References(ctx(t), doc, []byte(useDeps), at(t, useDeps, "size();", 0, 1), false)
	if err != nil || len(refs) != 1 || refs[0].File != filepath.Join(root, "src/main/java/com/acme/UseDeps.java") {
		t.Errorf("references of Container.size = %+v %v", refs, err)
	}
	if treeHash(t, repo) != before {
		t.Errorf("the local repository changed")
	}
}

// A dependency added to pom.xml is found by the next scan, and a dependency
// with a sources jar opens its source.
func TestEditingPOMRereadsTheDependencies(t *testing.T) {
	root := t.TempDir()
	src := "package com.acme;\n\nimport com.example.helper.Helper;\n\nclass A {\n    String s = Helper.shout(\"x\");\n}\n"
	write(t, root, map[string]string{"pom.xml": depsPOM, "src/main/java/com/acme/A.java": src})
	w := New(Options{Root: root, State: filepath.Join(t.TempDir(), "state"), Dependencies: true, MavenRepo: mavenRepo(t),
		ListFiles: walkFiles, Freshness: -1, Logf: t.Logf})
	doc := w.DocOf(filepath.Join(root, "src/main/java/com/acme/A.java"))
	if _, ok, err := w.Definition(ctx(t), doc, []byte(src), at(t, src, "shout", 0, 1)); err != nil || ok {
		t.Fatalf("Helper resolved before the POM declared it: %v %v", ok, err)
	}
	pom := strings.Replace(depsPOM, "</dependencies>",
		"  <dependency><groupId>com.example</groupId><artifactId>helper</artifactId><version>2.0</version></dependency>\n</dependencies>", 1)
	// A size change is enough for the scan to notice, whatever the clock's
	// resolution.
	write(t, root, map[string]string{"pom.xml": pom})
	tgt, ok, err := w.Definition(ctx(t), doc, []byte(src), at(t, src, "shout", 0, 1))
	if err != nil || !ok {
		t.Fatalf("Helper.shout after the POM changed: %v %v", ok, err)
	}
	b, err := os.ReadFile(tgt.File)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(string(b), "\n"); !strings.Contains(lines[tgt.Line], "public static String shout(String text) {") ||
		!strings.Contains(string(b), "return text.toUpperCase();") {
		t.Errorf("shout opened line %d of\n%s", tgt.Line, b)
	}
}

// Sources a build generated into a module's output directory are listed,
// though .gitignore and the walk leave target/ out.
func TestGeneratedSourcesAreListed(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"pom.xml":     "<project/>",
		"api/pom.xml": "<project/>",
		"api/target/generated-sources/protobuf/java/a/Msg.java":                "package a; public class Msg {}",
		"api/target/classes/a/Msg.class":                                       "",
		"api/target/other/B.java":                                              "package b; class B {}",
		"web/build.gradle":                                                     "",
		"web/build/generated/sources/annotationProcessor/java/main/w/Gen.java": "package w; class Gen {}",
		"src/main/java/a/Use.java":                                             "package a; class Use { Msg m; }",
	})
	files, err := ListFiles(ctx(t), root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, want := range []string{"api/target/generated-sources/protobuf/java/a/Msg.java",
		"web/build/generated/sources/annotationProcessor/java/main/w/Gen.java", "src/main/java/a/Use.java"} {
		if !got[want] {
			t.Errorf("%s not listed in %v", want, files)
		}
	}
	if got["api/target/other/B.java"] || got["api/target/classes/a/Msg.class"] {
		t.Errorf("listed other build output: %v", files)
	}
}

// A project without Maven, its jars in WEB-INF/lib as an Ant or Eclipse web
// application keeps them, navigates into them; a jar added to lib/ is found
// by the next scan.
func TestDefinitionIntoJarsTheTreeKeeps(t *testing.T) {
	root := t.TempDir()
	src := "package com.acme;\n\nimport com.example.lib.Container;\nimport com.example.helper.Helper;\n\nclass A {\n    Container<String> box;\n    String s = Helper.shout(\"x\");\n}\n"
	write(t, root, map[string]string{"build.xml": "<project/>", "src/com/acme/A.java": src})
	fixture := filepath.Join("..", "deps", "testdata", "fixture")
	if err := os.MkdirAll(filepath.Join(root, "WEB-INF", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(fixture, "lib-1.0.jar"), filepath.Join(root, "WEB-INF", "lib", "lib-1.0.jar"))
	before := treeHash(t, filepath.Join(root, "WEB-INF"))
	w := New(Options{Root: root, State: filepath.Join(t.TempDir(), "state"), Dependencies: true,
		ListFiles: walkFiles, Freshness: -1, Logf: t.Logf})
	doc := w.DocOf(filepath.Join(root, "src/com/acme/A.java"))
	tgt, ok, err := w.Definition(ctx(t), doc, []byte(src), at(t, src, "Container<String>", 0, 1))
	if err != nil || !ok || !strings.Contains(filepath.ToSlash(tgt.File), "/state/dependency-sources/lib-1.0-") {
		t.Fatalf("Container: %+v %v %v", tgt, ok, err)
	}
	if _, ok, err := w.Definition(ctx(t), doc, []byte(src), at(t, src, "shout", 0, 1)); err != nil || ok {
		t.Fatalf("Helper resolved before its jar was added: %v %v", ok, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(fixture, "helper-2.0.jar"), filepath.Join(root, "lib", "helper-2.0.jar"))
	copyFile(t, filepath.Join(fixture, "helper-2.0-sources.jar"), filepath.Join(root, "lib", "helper-2.0-sources.jar"))
	tgt, ok, err = w.Definition(ctx(t), doc, []byte(src), at(t, src, "shout", 0, 1))
	if err != nil || !ok {
		t.Fatalf("Helper.shout after the jar was added: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(tgt.File); !strings.Contains(string(b), "return text.toUpperCase();") {
		t.Errorf("shout did not open the sources jar:\n%s", b)
	}
	if treeHash(t, filepath.Join(root, "WEB-INF")) != before {
		t.Errorf("WEB-INF changed")
	}
}
