package deps

import (
	"encoding/xml"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Projects older than Maven, and many built with Ant or exported from
// Eclipse or MyEclipse, keep their jars in the working tree instead of
// resolving them from a repository: WEB-INF/lib of a web application, a lib
// directory the build.xml adds to the classpath, the library entries of an
// Eclipse .classpath file. TreeJars finds those jars.

// maxTreeJars bounds the jars one working tree contributes.
const maxTreeJars = 2000

// libDirs are the names of directories whose jars are on a build's
// classpath. A jar counts when one of the three directories above it has
// such a name, so lib/optional/junit.jar and WEB-INF/lib/x.jar do, and a
// test's resources jar or gradle/wrapper/gradle-wrapper.jar do not.
var libDirs = map[string]bool{"lib": true, "libs": true}

// skippedTreeDirs hold build output and copies of jars found elsewhere.
var skippedTreeDirs = map[string]bool{"target": true, "build": true, "out": true, "bin": true, "node_modules": true, ".git": true}

// libProbes are directories, relative to the tree's root and to each
// directory holding a build.xml, .classpath or .project, whose jars are read
// even when .gitignore leaves them out of the listing (Ant and Ivy often
// download them there).
var libProbes = []string{"lib", "libs", "WEB-INF/lib", "WebContent/WEB-INF/lib", "WebRoot/WEB-INF/lib",
	"web/WEB-INF/lib", "src/main/webapp/WEB-INF/lib"}

// projectMarkers name the files whose directory is probed for libProbes.
var projectMarkers = map[string]bool{"build.xml": true, ".classpath": true, ".project": true}

// TreeJars lists the jars root keeps in its working tree: the library
// entries of its Eclipse .classpath files, in their order, then the jars in
// lib directories (see libDirs and libProbes). files are root's files as
// slash-separated relative paths; repo resolves Eclipse's M2_REPO variable
// and may be "". Jars that do not exist are left out. A jar's sources are the
// .classpath entry's sourcepath, else a -sources.jar or -src.zip next to it.
// Last come the jars of the .classpath's server runtime and JUnit containers
// (see container), when the tree does not keep them itself.
func TreeJars(root string, files []string, repo string) []Jar {
	t := treeJars{seen: map[string]bool{}}
	var markerDirs []string
	listed := map[string]bool{}
	for _, f := range files {
		listed[f] = true
		dir, base := path.Split(f)
		if base == ".classpath" {
			t.classpath(root, f, repo)
		}
		if projectMarkers[base] {
			markerDirs = append(markerDirs, strings.TrimSuffix(dir, "/"))
		}
	}
	for _, f := range files {
		if inLibDir(f) {
			t.add(filepath.Join(root, filepath.FromSlash(f)), "")
		}
	}
	probed := map[string]bool{}
	for _, d := range append([]string{""}, markerDirs...) {
		for _, p := range libProbes {
			dir := path.Join(d, p)
			if !probed[dir] {
				probed[dir] = true
				t.walkLib(filepath.Join(root, filepath.FromSlash(dir)), 0)
			}
		}
	}
	if t.servlet == "" {
		t.servlet, _ = webAppWithoutBuild(root, files)
	}
	t.resolveContainers(repo)
	return t.out
}

type treeJars struct {
	out  []Jar
	seen map[string]bool
	// servlet is the package of the Servlet API a server runtime provides,
	// "" when the tree uses none; junit is the JUnit container's version,
	// "" when there is none. resolveContainers adds them last.
	servlet string
	junit   string
}

// add lists jar once, with sources when given or found next to it.
func (t *treeJars) add(jar, sources string) {
	t.addAs(jar, sources, strings.TrimSuffix(filepath.Base(jar), ".jar"))
}

// addAs is add with the jar's coordinates, and reports whether it listed it.
func (t *treeJars) addAs(jar, sources, coord string) bool {
	jar = filepath.Clean(jar)
	if len(t.out) >= maxTreeJars || t.seen[jar] || !isLibraryJar(filepath.Base(jar)) {
		return false
	}
	if fi, err := os.Stat(jar); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	t.seen[jar] = true
	if sources == "" {
		sources = sourcesNextTo(jar)
	} else if fi, err := os.Stat(sources); err != nil || !fi.Mode().IsRegular() {
		sources = ""
	}
	t.out = append(t.out, Jar{Coord: coord, Path: jar, Sources: sources})
	return true
}

// walkLib adds the jars under dir, two directory levels deep.
func (t *treeJars) walkLib(dir string, depth int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() && depth < 2 && !skippedTreeDirs[e.Name()] && !strings.HasPrefix(e.Name(), "."):
			t.walkLib(p, depth+1)
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jar"):
			t.add(p, "")
		}
	}
}

// classpathFile is the part of an Eclipse .classpath that names jars.
type classpathFile struct {
	Entries []struct {
		Kind       string `xml:"kind,attr"`
		Path       string `xml:"path,attr"`
		SourcePath string `xml:"sourcepath,attr"`
	} `xml:"classpathentry"`
}

// classpath adds the lib and M2_REPO var entries of the .classpath file rel.
func (t *treeJars) classpath(root, rel, repo string) {
	b, err := readLimited(filepath.Join(root, filepath.FromSlash(rel)), maxPOMSize)
	if err != nil {
		return
	}
	var cp classpathFile
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	dec.CharsetReader = charsetReader
	if dec.Decode(&cp) != nil {
		return
	}
	project := filepath.Join(root, filepath.FromSlash(path.Dir(rel)))
	resolve := func(kind, p string) string {
		if p == "" {
			return ""
		}
		switch kind {
		case "var":
			v, rest, _ := strings.Cut(filepath.ToSlash(p), "/")
			if v != "M2_REPO" || repo == "" || rest == "" {
				return ""
			}
			return filepath.Join(repo, filepath.FromSlash(rest))
		case "lib":
			if filepath.IsAbs(p) {
				if _, err := os.Stat(p); err == nil {
					return p
				}
				// "/Other/lib/x.jar" names a jar of the Eclipse project Other,
				// usually a sibling folder.
				return filepath.Join(filepath.Dir(project), filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(p), "/")))
			}
			return filepath.Join(project, filepath.FromSlash(p))
		}
		return ""
	}
	for _, e := range cp.Entries {
		if e.Kind == "con" {
			t.container(e.Path)
			continue
		}
		if jar := resolve(e.Kind, e.Path); jar != "" {
			t.add(jar, resolve(e.Kind, e.SourcePath))
		}
	}
}

// inLibDir reports whether the listed file rel is a jar in a lib directory
// and not under build output.
func inLibDir(rel string) bool {
	if !strings.HasSuffix(rel, ".jar") {
		return false
	}
	dirs := strings.Split(path.Dir(rel), "/")
	for _, d := range dirs {
		if skippedTreeDirs[d] {
			return false
		}
	}
	for i := len(dirs) - 1; i >= 0 && i >= len(dirs)-3; i-- {
		if libDirs[dirs[i]] {
			return true
		}
	}
	return false
}

// isLibraryJar leaves out the source and documentation jars that sit next
// to the jars they describe.
func isLibraryJar(name string) bool {
	stem, ok := strings.CutSuffix(name, ".jar")
	if !ok {
		return false
	}
	for _, s := range []string{"-sources", "-src", "-javadoc"} {
		if strings.HasSuffix(stem, s) {
			return false
		}
	}
	return true
}

// readLimited reads the regular file p when it is at most limit bytes.
func readLimited(p string, limit int64) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, os.ErrInvalid
	}
	return os.ReadFile(p)
}

// sourcesNextTo finds the source archive a tree keeps next to jar.
func sourcesNextTo(jar string) string {
	stem := strings.TrimSuffix(jar, ".jar")
	for _, s := range []string{"-sources.jar", "-src.jar", "-sources.zip", "-src.zip"} {
		if fi, err := os.Stat(stem + s); err == nil && fi.Mode().IsRegular() {
			return stem + s
		}
	}
	return ""
}
