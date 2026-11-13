package codenav

import (
	"fmt"
	"strings"
	"sync"
)

// Library is a read-only set of Java sources outside the working tree: the
// JDK's src.zip and the project's dependency jars (their sources, or stubs
// written from their class files). Unlike the Index it is not parsed up front. A lookup the
// working tree cannot answer asks the library for the type's fully qualified
// name, and the source declaring it is read and parsed the first time, then
// kept, so a session only ever holds the JDK classes it navigated through.
//
// Library paths are opaque to this package: whatever the reader understands
// (an entry name in the archive). They live in their own namespace, marked by
// File.Lib and Location.Library, so a working-tree file with the same path
// never stands in for a library one.
//
// A Library is safe for concurrent use and can be shared by several Indexes.
type Library struct {
	// paths maps each top-level type to the library path of its source,
	// keyed by the type's slash-separated name ("java/lang/String"), which is
	// a substring of the path, so a library of 150,000 classes holds one
	// string per class; it never changes, so misses need no lock.
	paths map[string]string
	read  func(path string) ([]byte, error)

	mu    sync.RWMutex
	files map[string]*File   // parsed sources by path; an empty File marks a failed read
	byFQN map[string][]*Type // every type of the parsed sources, nested ones too
	err   error              // the latest read failure not reported yet
}

// NewLibrary returns a library over the sources at paths; the FQN of each
// one's top-level type follows from its path (see LibraryFQN), and paths that
// declare no type are ignored. When several paths declare the same type the
// first one wins, so callers list the JDK before the dependencies and those
// in classpath order. read returns a source's content; it is called at most
// once per path.
func NewLibrary(paths []string, read func(path string) ([]byte, error)) *Library {
	l := &Library{
		paths: make(map[string]string, len(paths)),
		read:  read,
		files: map[string]*File{},
		byFQN: map[string][]*Type{},
	}
	for _, p := range paths {
		if key, ok := libraryKey(p); ok {
			if _, dup := l.paths[key]; !dup {
				l.paths[key] = p
			}
		}
	}
	return l
}

// LibraryFQN derives the fully qualified name of the top-level type a source
// archive entry declares: "java.base/java/lang/String.java" (JDK 9 and later,
// led by a module directory) and "java/lang/String.java" (JDK 8) both give
// "java.lang.String". An entry of a dependency jar is led by the jar's key and
// a "!" ("<key>!a/b/C.java"). module-info and package-info declare no type.
func LibraryFQN(entry string) (string, bool) {
	key, ok := libraryKey(entry)
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(key, "/", "."), true
}

// libraryKey is LibraryFQN with slashes for dots, as a substring of entry.
func libraryKey(entry string) (string, bool) {
	name, ok := strings.CutSuffix(entry, ".java")
	if !ok || name == "" {
		return "", false
	}
	if i := strings.IndexByte(name, '!'); i >= 0 {
		name = name[i+1:]
	} else if i := strings.IndexByte(name, '/'); i > 0 && strings.Contains(name[:i], ".") {
		// A module directory: package directories never contain a dot.
		name = name[i+1:]
	}
	base := name[strings.LastIndexByte(name, '/')+1:]
	if base == "module-info" || base == "package-info" || strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

// Len is the number of sources parsed so far.
func (l *Library) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.files)
}

// Err returns the latest failure to read a source, once: a lookup that needed
// it answered as if the type did not exist, and the caller decides how to tell
// the user.
func (l *Library) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.err
	l.err = nil
	return err
}

// lookup returns the type named fqn, reading and parsing the source that
// declares it (or declares its outermost type, for a nested one) on first use.
func (l *Library) lookup(fqn string) *Type {
	src := ""
	for top := fqn; ; {
		if p, ok := l.paths[strings.ReplaceAll(top, ".", "/")]; ok {
			src = p
			break
		}
		i := strings.LastIndexByte(top, '.')
		if i < 0 {
			return nil // not a library type: the common case, lock-free
		}
		top = top[:i]
	}
	l.mu.RLock()
	_, loaded := l.files[src]
	ts := l.byFQN[fqn]
	l.mu.RUnlock()
	if !loaded {
		l.load(src)
		l.mu.RLock()
		ts = l.byFQN[fqn]
		l.mu.RUnlock()
	}
	if len(ts) == 0 {
		return nil
	}
	return ts[0]
}

// load parses the source at path unless another goroutine did meanwhile.
func (l *Library) load(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.files[path]; ok {
		return
	}
	f := &File{Path: path, Imports: map[string]string{}, Lib: true}
	if src, err := l.read(path); err != nil {
		l.err = fmt.Errorf("read library source %s: %w", path, err)
	} else {
		f = parseJava(src, path, newInterner(), false).f
		f.Lib = true
	}
	l.files[path] = f
	var walk func(t *Type)
	walk = func(t *Type) {
		l.byFQN[t.FQN] = append(l.byFQN[t.FQN], t)
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range f.Types {
		walk(t)
	}
}

// file returns the parsed source at path, parsing it if a lookup has not yet.
func (l *Library) file(path string) *File {
	l.mu.RLock()
	f, ok := l.files[path]
	l.mu.RUnlock()
	if ok {
		return f
	}
	if key, ok := libraryKey(path); !ok || l.paths[key] != path {
		return nil
	}
	l.load(path)
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.files[path]
}
