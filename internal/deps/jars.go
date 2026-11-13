// Package deps lets navigation follow names into a project's dependency
// jars. It finds the jars of the Maven projects in a working tree in the
// local repository (MavenJars), lists the classes each one declares and,
// for a class a lookup needs, returns Java source: the entry of the jar's
// -sources.jar when it was downloaded, else a stub of signatures written
// from the class files. Only the archives' directories are read up front;
// codenav parses one class the first time a lookup needs it, so memory
// grows with the classes a session navigates through, not with the
// classpath.
//
// Navigation opens a declaration as a read-only copy of that source under
// the state directory the host gives the plugin, never in the local
// repository or the plugin package.
package deps

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/codenav"
)

// MaxEntrySize skips class files and sources too large to read.
const MaxEntrySize = 4 << 20

// stubFormat names the output of the stub writer; change it with the writer,
// so copies extracted by an older version are not reused.
const stubFormat = "1"

// ErrChanged reports a jar that changed since it was listed.
var ErrChanged = errors.New("the dependency jar changed on disk")

// Sep separates a jar's id from the entry in a library path:
// "<id>!a/b/C.java". Ids are numbers of one Set.
const Sep = "!"

// Set is the listed classes of a project's dependency jars. It never changes
// after Open, so its methods run on any goroutine.
type Set struct {
	jars  []*archive
	byID  map[string]*archive
	byDir map[string]*archive
	paths []string // library paths of every listed top-level class
}

// archive is one listed jar.
type archive struct {
	Jar
	id      string // names the jar in library paths: short, as there is one path per class
	dir     string // names the jar's directory of copies after its coordinates and identity
	file    string // the archive read: the sources jar when there is one
	sources bool
	size    int64
	mod     time.Time
}

// Open lists the jars, skipping those that cannot be read; an empty Set is
// not an error. logf reports skipped jars.
func Open(jars []Jar, logf func(string, ...any)) *Set {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Set{byID: map[string]*archive{}, byDir: map[string]*archive{}}
	listed := make([]*archive, len(jars))
	entries := make([][]string, len(jars))
	errs := make([]error, len(jars))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), 8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				listed[i], entries[i], errs[i] = listJar(jars[i])
			}
		}()
	}
	for i := range jars {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, a := range listed {
		if errs[i] != nil {
			logf("skipping %s: %v", jars[i].Path, errs[i])
			continue
		}
		if _, dup := s.byDir[a.dir]; dup {
			continue
		}
		a.id = strconv.FormatInt(int64(len(s.jars)), 36)
		s.jars = append(s.jars, a)
		s.byID[a.id] = a
		s.byDir[a.dir] = a
		for _, e := range entries[i] {
			s.paths = append(s.paths, a.id+Sep+e)
		}
	}
	return s
}

// Len is the number of listed jars.
func (s *Set) Len() int { return len(s.jars) }

// Classes is the number of listed top-level classes.
func (s *Set) Classes() int { return len(s.paths) }

// Paths returns the library path of every listed class, in classpath order:
// the first jar that declares a class is the one a lookup finds.
func (s *Set) Paths() []string { return s.paths }

// listJar lists the top-level classes of j: the .java entries of its
// sources jar when it has one that opens, else its .class entries, named
// as the .java file a stub of them is.
func listJar(j Jar) (*archive, []string, error) {
	a := &archive{Jar: j}
	if j.Sources != "" {
		a.file, a.sources = j.Sources, true
		if entries, err := a.list(); err == nil && len(entries) > 0 {
			return a, entries, nil
		}
	}
	a.file, a.sources = j.Path, false
	entries, err := a.list()
	if err != nil {
		return nil, nil, err
	}
	return a, entries, nil
}

func (a *archive) list() ([]string, error) {
	zr, err := zip.OpenReader(a.file)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	fi, err := os.Stat(a.file)
	if err != nil {
		return nil, err
	}
	a.size, a.mod = fi.Size(), fi.ModTime()
	id := a.file + "\x00" + strconv.FormatInt(a.size, 10) + "\x00" + a.mod.UTC().Format(time.RFC3339Nano)
	if !a.sources {
		id += "\x00stub " + stubFormat // a new stub writer never reuses older copies
	}
	h := sha256.Sum256([]byte(id))
	a.dir = sanitize(strings.ReplaceAll(a.Coord, ":", "_")) + "-" + hex.EncodeToString(h[:6])
	var out []string
	for _, f := range zr.File {
		name := f.Name
		if strings.HasPrefix(name, "META-INF/") || f.FileInfo().IsDir() || !validEntry(name) {
			continue
		}
		base := path.Base(name)
		if base == "module-info.class" || base == "package-info.class" || base == "module-info.java" || base == "package-info.java" {
			continue
		}
		if a.sources {
			if strings.HasSuffix(name, ".java") {
				out = append(out, name)
			}
			continue
		}
		stem, ok := strings.CutSuffix(name, ".class")
		if !ok || strings.Contains(path.Base(stem), "$") {
			continue // member, local and anonymous classes are written into their top-level class's stub
		}
		out = append(out, stem+".java")
	}
	return out, nil
}

// Has reports whether p is a library path of this set.
func (s *Set) Has(p string) bool {
	a, entry, ok := s.split(p)
	return ok && a != nil && entry != ""
}

func (s *Set) split(p string) (*archive, string, bool) {
	id, entry, ok := strings.Cut(p, Sep)
	if !ok {
		return nil, "", false
	}
	a := s.byID[id]
	if a == nil || !validEntry(entry) || !strings.HasSuffix(entry, ".java") {
		return nil, "", false
	}
	return a, entry, true
}

// Read returns the source of library path p: the sources jar's entry, or the
// stub of the class and its member classes.
func (s *Set) Read(p string) ([]byte, error) {
	a, entry, ok := s.split(p)
	if !ok {
		return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	zr, err := zip.OpenReader(a.file)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if fi, err := os.Stat(a.file); err != nil || fi.Size() != a.size || !fi.ModTime().Equal(a.mod) {
		return nil, ErrChanged
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		if f.UncompressedSize64 > MaxEntrySize {
			return nil, fmt.Errorf("%s: larger than %d bytes", name, MaxEntrySize)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, MaxEntrySize+1))
	}
	if a.sources {
		return read(entry)
	}
	stem := strings.TrimSuffix(entry, ".java")
	b, err := read(stem + ".class")
	if err != nil {
		return nil, err
	}
	top, err := parseClass(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stem+".class", err)
	}
	load := func(binary string) (*classFile, bool) {
		b, err := read(binary + ".class")
		if err != nil {
			return nil, false
		}
		c, err := parseClass(b)
		return c, err == nil
	}
	exists := func(binary string) bool { return files[binary+".class"] != nil }
	return writeStub(top, filepath.Base(a.Path), load, exists), nil
}

// CacheDir is the directory under state that holds the copies of the sources
// of every dependency.
func CacheDir(state string) string { return filepath.Join(state, "dependency-sources") }

// Extract writes a read-only copy of library path p under CacheDir(state)
// and returns its absolute path. An existing copy is reused: its directory is
// named after the jar's identity (path, size and modification time), so a
// changed jar never reuses an old copy. The copy is written to a temporary
// file first and renamed into place, so a reader never sees half a file.
func (s *Set) Extract(state, p string) (string, error) {
	a, entry, ok := s.split(p)
	if !ok {
		return "", fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	dst := filepath.Join(CacheDir(state), a.dir, filepath.FromSlash(entry))
	if fi, err := os.Stat(dst); err == nil && fi.Mode().IsRegular() {
		return dst, nil
	}
	b, err := s.Read(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("extract dependency source: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".extract-*")
	if err != nil {
		return "", fmt.Errorf("extract dependency source: %w", err)
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("extract dependency source: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("extract dependency source: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		// Another request may have extracted the same entry; Windows refuses
		// to replace a read-only file.
		if fi, serr := os.Stat(dst); serr == nil && fi.Mode().IsRegular() {
			return dst, nil
		}
		return "", fmt.Errorf("extract dependency source: %w", err)
	}
	return dst, nil
}

// PathOf maps file, an absolute path, back to the library path it is a copy
// of, when it lies under CacheDir(state).
func (s *Set) PathOf(state, file string) (string, bool) {
	rel, err := filepath.Rel(CacheDir(state), file)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	dir, entry, ok := strings.Cut(filepath.ToSlash(rel), "/")
	a := s.byDir[dir]
	if !ok || a == nil {
		return "", false
	}
	p := a.id + Sep + entry
	return p, s.Has(p)
}

// FQN derives the fully qualified name of the class library path p declares.
func FQN(p string) (string, bool) {
	_, entry, ok := strings.Cut(p, Sep)
	if !ok {
		return "", false
	}
	return codenav.LibraryFQN(entry)
}

// validEntry refuses entry names that would escape the cache directory.
func validEntry(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, ":") || strings.Contains(name, Sep) {
		return false
	}
	return path.Clean(name) == name && !strings.HasPrefix(name, "../") && name != ".."
}

// sanitize keeps a coordinate usable as a directory name.
func sanitize(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, v)
}
