package deps

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/codenav"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// A jar with a sources jar is read from the sources jar.
func TestTheSourcesJarIsReadWhenItWasDownloaded(t *testing.T) {
	s := fixtureSet(t)
	b, err := s.Read(pathOf(t, s, "com/example/helper/Helper.java"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "return text.toUpperCase();") {
		t.Fatalf("Helper source = %s", b)
	}
}

// Stubs parse into the declarations of their class files: member classes,
// enum constants, record components and annotation elements.
func TestStubsParseIntoTheirDeclarations(t *testing.T) {
	s := fixtureSet(t)
	parse := func(name string) *codenav.File {
		p := pathOf(t, s, "com/example/lib/"+name+".java")
		b, err := s.Read(p)
		if err != nil {
			t.Fatal(err)
		}
		f := codenav.ParseFile(p, b)
		if f.Pkg != "com.example.lib" || len(f.Types) != 1 || f.Types[0].Name != name {
			t.Fatalf("%s parsed as %+v", name, f)
		}
		return f
	}
	names := func(t *codenav.Type) []string {
		var out []string
		for _, m := range t.Members {
			out = append(out, m.Name)
		}
		for _, n := range t.Nested {
			out = append(out, n.Name+"{}")
		}
		return out
	}
	for name, want := range map[string]string{
		"Schema":     "description requiredMode implementation allowableValues maxLength RequiredMode{}",
		"Container":  "LIMIT NAME BIG index Container Container get size max entry Builder{} Cursor{}",
		"Level":      "LOW HIGH weight label",
		"Repository": "findById findAll exists identity",
	} {
		got := strings.Join(names(parse(name).Types[0]), " ")
		if got != want {
			t.Errorf("%s declares %q, want %q", name, got, want)
		}
	}
	mode := parse("Schema").Types[0].Nested[0]
	if got := strings.Join(names(mode), " "); got != "AUTO REQUIRED NOT_REQUIRED" {
		t.Errorf("RequiredMode declares %q", got)
	}
}

// A copy is written once under the state directory, read-only, and maps back
// to its library path; the jar is never written.
func TestExtractWritesOneReadOnlyCopyPerClass(t *testing.T) {
	jar := filepath.Join("testdata", "fixture", "lib-1.0.jar")
	before := fileHash(t, jar)
	s := fixtureSet(t)
	state := t.TempDir()
	p := pathOf(t, s, "com/example/lib/Container.java")
	file, err := s.Extract(state, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(file, CacheDir(state)+string(filepath.Separator)) ||
		!strings.HasSuffix(filepath.ToSlash(file), "/com/example/lib/Container.java") {
		t.Fatalf("copy at %s", file)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.Read(p)
	if string(b) != string(want) {
		t.Errorf("copy differs from the stub")
	}
	if fi, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("copy mode = %v, %v", fi.Mode(), err)
	}
	again, err := s.Extract(state, p)
	if err != nil || again != file {
		t.Errorf("second Extract = %s, %v", again, err)
	}
	if back, ok := s.PathOf(state, file); !ok || back != p {
		t.Errorf("PathOf(copy) = %s %v, want %s", back, ok, p)
	}
	if _, ok := s.PathOf(state, filepath.Join(state, "elsewhere", "A.java")); ok {
		t.Errorf("PathOf accepted a file outside the copies")
	}
	if fileHash(t, jar) != before {
		t.Errorf("the jar changed")
	}
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// Truncated or corrupted class files are rejected without a panic.
func TestBrokenClassFilesAreRejected(t *testing.T) {
	zr, err := zip.OpenReader(filepath.Join("testdata", "fixture", "lib-1.0.jar"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".class") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, f.UncompressedSize64)
		if _, err := rc.Read(b); err != nil && len(b) == 0 {
			t.Fatal(err)
		}
		rc.Close()
		if _, err := parseClass(b); err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		for n := 0; n < len(b); n += 7 {
			if _, err := parseClass(b[:n]); err == nil {
				t.Errorf("%s truncated to %d bytes parsed", f.Name, n)
			}
		}
		for i := 10; i < len(b); i += 13 {
			c := append([]byte(nil), b...)
			c[i] ^= 0xff
			parseClass(c) // must not panic
		}
	}
}

func TestVersionsOrderLikeMaven(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2", "1.10", -1},
		{"1.0", "1", 0},
		{"1.0.0.RELEASE", "1.0", 0},
		{"2.0-SNAPSHOT", "2.0", -1},
		{"2.0-rc1", "2.0", -1},
		{"2.0-beta", "2.0-rc1", -1},
		{"2.0.1", "2.0", 1},
		{"5.8.48", "5.8.9", 1},
		{"1.0.Final", "1.0", 0},
		{"1.0-sp1", "1.0", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(c.b, c.a); got != -c.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// repo is a local Maven repository for a test.
type repo struct {
	t    *testing.T
	root string
}

func newRepo(t *testing.T) *repo { return &repo{t: t, root: t.TempDir()} }

// add writes group:artifact:version's POM with the given body (inside
// <project>) and, unless body is a pom packaging, a jar.
func (r *repo) add(coord, body string, jar bool) string {
	r.t.Helper()
	parts := strings.Split(coord, ":")
	g, a, v := parts[0], parts[1], parts[2]
	dir := filepath.Join(r.root, filepath.FromSlash(strings.ReplaceAll(g, ".", "/")), a, v)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	pom := "<project><modelVersion>4.0.0</modelVersion><groupId>" + g + "</groupId><artifactId>" + a +
		"</artifactId><version>" + v + "</version>" + body + "</project>"
	if err := os.WriteFile(filepath.Join(dir, a+"-"+v+".pom"), []byte(pom), 0o644); err != nil {
		r.t.Fatal(err)
	}
	if jar {
		path := filepath.Join(dir, a+"-"+v+".jar")
		writeJar(r.t, path, map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n"})
		return path
	}
	return ""
}

func writeJar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func dep(coord string, extra ...string) string {
	parts := strings.Split(coord, ":")
	s := "<dependency><groupId>" + parts[0] + "</groupId><artifactId>" + parts[1] + "</artifactId>"
	if len(parts) > 2 && parts[2] != "" {
		s += "<version>" + parts[2] + "</version>"
	}
	return s + strings.Join(extra, "") + "</dependency>"
}

func coords(jars []Jar) string {
	var out []string
	for _, j := range jars {
		out = append(out, j.Coord)
	}
	return strings.Join(out, " ")
}

// Dependencies are mediated as Maven does: the parent's properties and
// management apply, an imported BOM manages versions, nearest wins, test,
// provided and optional dependencies are not transitive, exclusions apply,
// and the working tree's own modules are not jars.
func TestMavenJarsFollowsMavenMediation(t *testing.T) {
	r := newRepo(t)
	r.add("org.bom:platform:1.0", `<packaging>pom</packaging><dependencyManagement><dependencies>`+
		dep("org.lib:core:2.0")+dep("org.lib:util:1.5")+`</dependencies></dependencyManagement>`, false)
	r.add("org.lib:core:2.0", `<dependencies>`+dep("org.lib:util:1.0")+dep("org.lib:junk:1.0", "<scope>test</scope>")+
		dep("org.lib:opt:1.0", "<optional>true</optional>")+dep("org.lib:dropped:1.0")+dep("org.lib:deep:1.0")+`</dependencies>`, true)
	r.add("org.lib:core:1.0", ``, true)
	r.add("org.lib:util:1.0", ``, true)
	r.add("org.lib:util:1.5", ``, true)
	r.add("org.lib:junk:1.0", ``, true)
	r.add("org.lib:opt:1.0", ``, true)
	r.add("org.lib:dropped:1.0", ``, true)
	r.add("org.lib:deep:1.0", `<dependencies>`+dep("org.lib:util:0.9")+`</dependencies>`, true)
	r.add("org.lib:servlet:4.0", ``, true)
	r.add("org.lib:newest:1.2", ``, true)
	r.add("org.lib:newest:1.10", ``, true)

	tree := t.TempDir()
	root := writeFile(t, filepath.Join(tree, "pom.xml"), `<project><groupId>com.acme</groupId><artifactId>parent</artifactId>
<version>3.0</version><packaging>pom</packaging>
<properties><platform.version>1.0</platform.version></properties>
<dependencyManagement><dependencies>`+dep("org.bom:platform:${platform.version}", "<type>pom</type><scope>import</scope>")+`
</dependencies></dependencyManagement></project>`)
	app := writeFile(t, filepath.Join(tree, "app", "pom.xml"), `<project><parent><groupId>com.acme</groupId><artifactId>parent</artifactId>
<version>3.0</version></parent><artifactId>app</artifactId><dependencies>`+
		dep("com.acme:model:${project.version}")+
		dep("org.lib:core", "<exclusions><exclusion><groupId>org.lib</groupId><artifactId>dropped</artifactId></exclusion></exclusions>")+
		dep("org.lib:servlet:4.0", "<scope>provided</scope>")+
		dep("org.lib:newest:${undefined.version}")+
		dep("org.lib:missing:1.0")+`</dependencies></project>`)
	model := writeFile(t, filepath.Join(tree, "model", "pom.xml"), `<project><parent><groupId>com.acme</groupId><artifactId>parent</artifactId>
<version>3.0</version></parent><artifactId>model</artifactId></project>`)

	jars, err := MavenJars(ctx(t), []string{root, app, model}, r.root, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	// core's version comes from the BOM, util from the BOM over core's 1.0
	// (the root's management applies to transitive dependencies), deep's
	// own util 0.9 loses to the nearer one; servlet is provided but direct.
	want := "org.lib:core:2.0 org.lib:servlet:4.0 org.lib:newest:1.10 org.lib:util:1.5 org.lib:deep:1.0"
	if got := coords(jars); got != want {
		t.Errorf("jars = %s\nwant   %s", got, want)
	}
	for _, j := range jars {
		if _, err := os.Stat(j.Path); err != nil || j.Sources != "" {
			t.Errorf("%s: path %s sources %q", j.Coord, j.Path, j.Sources)
		}
	}
}

// A POM in ISO-8859-1, as many Apache POMs are, is read.
func TestLatin1POMsAreRead(t *testing.T) {
	r := newRepo(t)
	r.add("org.lib:a:1.0", "", true)
	pom := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<project><groupId>com.acme</groupId><artifactId>caf\xe9</artifactId>" +
		"<version>1</version><name>caf\xe9</name><dependencies>" + dep("org.lib:a:1.0") + "</dependencies></project>"
	tree := t.TempDir()
	path := writeFile(t, filepath.Join(tree, "pom.xml"), pom)
	jars, err := MavenJars(ctx(t), []string{path}, r.root, t.Logf)
	if err != nil || coords(jars) != "org.lib:a:1.0" {
		t.Fatalf("jars = %v, %v", coords(jars), err)
	}
}

// A sources jar next to the jar is used.
func TestSourcesJarsAreFoundNextToTheJar(t *testing.T) {
	r := newRepo(t)
	jar := r.add("org.lib:a:1.0", "", true)
	writeJar(t, strings.TrimSuffix(jar, ".jar")+"-sources.jar", map[string]string{"org/lib/A.java": "package org.lib; public class A {}"})
	tree := t.TempDir()
	path := writeFile(t, filepath.Join(tree, "pom.xml"), `<project><groupId>x</groupId><artifactId>y</artifactId><version>1</version><dependencies>`+
		dep("org.lib:a:1.0")+`</dependencies></project>`)
	jars, err := MavenJars(ctx(t), []string{path}, r.root, t.Logf)
	if err != nil || len(jars) != 1 || !strings.HasSuffix(jars[0].Sources, "a-1.0-sources.jar") {
		t.Fatalf("jars = %+v, %v", jars, err)
	}
	s := Open(jars, t.Logf)
	if s.Classes() != 1 || !strings.HasSuffix(s.Paths()[0], Sep+"org/lib/A.java") {
		t.Errorf("paths = %v", s.Paths())
	}
}

func TestLocalRepositoryHonoursSettings(t *testing.T) {
	home := t.TempDir()
	if got := LocalRepository(home); got != filepath.Join(home, ".m2", "repository") {
		t.Errorf("default = %s", got)
	}
	custom := filepath.Join(home, "repo")
	writeFile(t, filepath.Join(home, ".m2", "settings.xml"), `<settings><localRepository>${user.home}/repo</localRepository></settings>`)
	if got := LocalRepository(home); filepath.Clean(got) != custom {
		t.Errorf("from settings = %s, want %s", got, custom)
	}
}
