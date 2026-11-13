package deps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeJar writes an empty jar at rel under root and returns rel.
func treeJar(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJar(t, p, map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n"})
	return rel
}

func relJars(t *testing.T, root string, jars []Jar) string {
	t.Helper()
	var out []string
	for _, j := range jars {
		rel, err := filepath.Rel(root, j.Path)
		if err != nil {
			t.Fatal(err)
		}
		s := filepath.ToSlash(rel)
		if j.Sources != "" {
			src, _ := filepath.Rel(root, j.Sources)
			s += "+" + filepath.ToSlash(src)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// A project without Maven keeps its jars in the tree: the .classpath entries
// come first, in their order, then the jars of lib directories, including
// those .gitignore leaves out of the listing; test resources, build output
// and the Gradle wrapper are not dependencies.
func TestTreeJarsOfAnAntAndEclipseProject(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	treeJar(t, repo, "org/x/m2lib/1.0/m2lib-1.0.jar")
	files := []string{
		treeJar(t, root, "WEB-INF/lib/c3p0.jar"),
		treeJar(t, root, "WEB-INF/lib/c3p0-sources.jar"),
		treeJar(t, root, "lib/servlet-api.jar"),
		treeJar(t, root, "lib/optional/junit-4.13.2.jar"),
		treeJar(t, root, "tools/src/lib/zz.jar"),
		treeJar(t, root, "src/etc/testcases/jars/signed.jar"),
		treeJar(t, root, "gradle/wrapper/gradle-wrapper.jar"),
		treeJar(t, root, "build/lib/app.jar"),
		treeJar(t, root, "srcs/mail-src.jar"),
		treeJar(t, root, "other/mail.jar"),
		"build.xml", ".classpath",
	}
	// Not listed: Ivy downloads into lib/ and .gitignore leaves it out.
	treeJar(t, root, "lib/ivy/commons-io.jar")
	writeFile(t, filepath.Join(root, ".classpath"), `<?xml version="1.0" encoding="UTF-8"?>
<classpath>
	<classpathentry kind="src" path="src"/>
	<classpathentry kind="lib" path="other/mail.jar" sourcepath="srcs/mail-src.jar"/>
	<classpathentry kind="lib" path="lib/servlet-api.jar"/>
	<classpathentry kind="var" path="M2_REPO/org/x/m2lib/1.0/m2lib-1.0.jar"/>
	<classpathentry kind="lib" path="lib/missing.jar"/>
	<classpathentry kind="con" path="org.eclipse.jdt.launching.JRE_CONTAINER"/>
</classpath>`)
	writeFile(t, filepath.Join(root, "build.xml"), "<project/>")

	got := TreeJars(root, files, repo)
	rel := relJars(t, root, got)
	m2 := filepath.ToSlash(filepath.Join(repo, "org/x/m2lib/1.0/m2lib-1.0.jar"))
	rel = strings.Replace(rel, mustRel(t, root, m2), "M2_REPO/m2lib-1.0.jar", 1)
	want := "other/mail.jar+srcs/mail-src.jar lib/servlet-api.jar M2_REPO/m2lib-1.0.jar " +
		"WEB-INF/lib/c3p0.jar+WEB-INF/lib/c3p0-sources.jar lib/optional/junit-4.13.2.jar tools/src/lib/zz.jar " +
		"lib/ivy/commons-io.jar"
	if rel != want {
		t.Errorf("TreeJars =\n %s\nwant\n %s", rel, want)
	}
	if got[0].Coord != "mail" {
		t.Errorf("coordinate of other/mail.jar = %q", got[0].Coord)
	}
}

func mustRel(t *testing.T, root, p string) string {
	t.Helper()
	r, err := filepath.Rel(root, filepath.FromSlash(p))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(r)
}

// A source archive of a source tree, not laid out by package, would name
// every class wrongly: the class jar's stubs are used instead.
func TestSourcesNotRootedAtThePackagesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	lib, err := os.ReadFile(filepath.Join("testdata", "fixture", "lib-1.0.jar"))
	if err != nil {
		t.Fatal(err)
	}
	jar := writeFile(t, filepath.Join(dir, "lib.jar"), string(lib))
	srcTree := filepath.Join(dir, "lib-src.zip")
	writeJar(t, srcTree, map[string]string{"lib-1.0/src/com/example/lib/Point.java": "package com.example.lib; public record Point(int x, int y) {}"})
	s := Open([]Jar{{Coord: "lib", Path: jar, Sources: srcTree}}, t.Logf)
	p := pathOf(t, s, "com/example/lib/Point.java")
	b, err := s.Read(p)
	if err != nil || !strings.Contains(string(b), "Signatures read from lib.jar") {
		t.Errorf("Point read %v:\n%s", err, b)
	}

	rooted := filepath.Join(dir, "lib-sources.jar")
	writeJar(t, rooted, map[string]string{"com/example/lib/Point.java": "package com.example.lib; public record Point(int x, int y) {}"})
	s = Open([]Jar{{Coord: "lib", Path: jar, Sources: rooted}}, t.Logf)
	if b, err := s.Read(pathOf(t, s, "com/example/lib/Point.java")); err != nil || !strings.HasPrefix(string(b), "package com.example.lib;") {
		t.Errorf("Point read from the sources %v:\n%s", err, b)
	}
}
