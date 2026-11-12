package codenav

import (
	"path"
	"sort"
	"strings"
	"sync"
)

// Supports reports whether path is a source file this package understands.
// Only Java is supported; the check is by extension, case-insensitive so a
// Windows checkout's "Foo.JAVA" is not silently skipped.
func Supports(p string) bool {
	return strings.EqualFold(path.Ext(p), ".java")
}

// Index is the resident declaration table of one working tree: for every
// parsed file its types and their members, and two lookups from a type's
// fully qualified name to its declarations. It holds no source
// text, tokens or syntax trees.
//
// Memory is proportional to the number of declarations. Measured on macOS
// arm64 with Go 1.27.3 (ADR 0040): Nacos, 5,366 files, about 14.5 MiB of
// retained heap; ruoyi-vue-pro, 7,673 files, about 19 MiB.
//
// Names the working tree does not declare are looked up in the Library set
// with SetLibrary, if any.
//
// An Index is safe for concurrent use: Put and Remove take a write lock,
// Resolve takes it only to replace the file it was given, and Usages and the
// other readers share a read lock, so a usage search can run on several
// goroutines while its owner keeps answering. ParseFile, which does the
// expensive part of indexing, takes no lock.
type Index struct {
	mu    sync.RWMutex
	files map[string]*File
	byFQN map[string][]*Type
	lib   *Library
}

// SetLibrary sets the library consulted for names the working tree does not
// declare; nil removes it.
func (ix *Index) SetLibrary(l *Library) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.lib = l
}

// fileOf returns the parsed file loc is in: an indexed one, or a library
// source. The caller holds the read lock.
func (ix *Index) fileOf(loc Location) *File {
	if !loc.Library {
		return ix.files[loc.Path]
	}
	if ix.lib == nil {
		return nil
	}
	return ix.lib.file(loc.Path)
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{files: map[string]*File{}, byFQN: map[string][]*Type{}}
}

// Len is the number of indexed files.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.files)
}

// Has reports whether path is indexed.
func (ix *Index) Has(path string) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.files[path]
	return ok
}

// Paths returns the indexed paths in lexical order.
func (ix *Index) Paths() []string {
	ix.mu.RLock()
	out := make([]string, 0, len(ix.files))
	for p := range ix.files {
		out = append(out, p)
	}
	ix.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Put adds a parsed file, replacing the declarations previously indexed under
// the same path.
func (ix *Index) Put(f *File) {
	if f == nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.remove(f.Path)
	ix.files[f.Path] = f
	var walk func(t *Type)
	walk = func(t *Type) {
		ix.byFQN[t.FQN] = append(ix.byFQN[t.FQN], t)
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range f.Types {
		walk(t)
	}
}

// Remove drops path's declarations; it is a no-op for an unknown path.
func (ix *Index) Remove(path string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.remove(path)
}

func (ix *Index) remove(path string) {
	f := ix.files[path]
	if f == nil {
		return
	}
	drop := func(m map[string][]*Type, k string, t *Type) {
		s := m[k]
		for i, x := range s {
			if x == t {
				s = append(s[:i:i], s[i+1:]...)
				break
			}
		}
		if len(s) == 0 {
			delete(m, k)
		} else {
			m[k] = s
		}
	}
	var walk func(t *Type)
	walk = func(t *Type) {
		drop(ix.byFQN, t.FQN, t)
		for _, c := range t.Nested {
			walk(c)
		}
	}
	for _, t := range f.Types {
		walk(t)
	}
	delete(ix.files, path)
}
