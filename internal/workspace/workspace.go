// Package workspace owns the declaration index of one project folder and
// answers go to definition, find references and outline queries over it.
//
// The index is a cache of declarations read from the folder (adapted from
// Viff's ADR 0040); it only speeds up a query and never replaces the files:
//
//   - It is built on the first query, not when the server starts, so a
//     project nobody navigates costs nothing.
//   - A query whose index is older than the freshness window rescans first:
//     the file list is read again and only files whose size or modification
//     time changed are parsed again, so edits made by other tools show up.
//   - Open documents are indexed from the text the editor holds, not from the
//     copy on disk, and the document a query comes from is parsed from that
//     text again.
//
// Names the folder does not declare resolve into the JDK's sources when a JDK
// with sources is installed, and into the jars of the folder's Maven
// dependencies that the local repository holds (package deps). Their
// declarations are extracted, one file at a time, into read-only copies
// under the state directory, which the editor opens as ordinary files; a
// query from such a copy is answered as a library source.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/codenav"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/deps"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/jdk"
)

const (
	// MaxFileSize skips generated sources too large to be read by hand.
	MaxFileSize = 2 << 20
	// scanTimeout bounds a scan; a first scan of a large project takes well
	// under a second on a developer machine (Viff ADR 0040).
	scanTimeout = 2 * time.Minute
	// DefaultFreshness is how long a scan answers queries before the next
	// query rescans.
	DefaultFreshness = 2 * time.Second
	// MaxReferences bounds a reference list.
	MaxReferences = 5000
	// releaseAfter is the number of files a scan parses before the memory
	// it allocated is returned to the system right away.
	releaseAfter = 500
	// depsRefresh is how long the dependency jars are used before a scan
	// reads the build files and the local repository again, so jars a build
	// downloaded meanwhile are found without a change to a pom.xml.
	depsRefresh = 5 * time.Minute
)

// Options configure a Workspace.
type Options struct {
	// Root is the absolute project folder.
	Root string
	// State is a writable directory for extracted JDK sources; "" disables
	// navigation into the JDK.
	State string
	// FindJDK locates the JDK whose sources are used; nil disables them.
	FindJDK func(context.Context) (jdk.JDK, bool)
	// MavenRepo is the local Maven repository whose jars the folder's
	// dependencies are read from; "" (or an empty State) disables them.
	MavenRepo string
	// ListFiles lists the files of root as slash-separated relative paths;
	// nil uses ListFiles.
	ListFiles func(ctx context.Context, root string) ([]string, error)
	// Freshness overrides DefaultFreshness; a negative value rescans before
	// every query.
	Freshness time.Duration
	// Logf reports problems that do not fail a query; nil discards them.
	Logf func(format string, args ...any)
}

// Doc names a document: a file of the workspace (Rel set), a library source
// (Library set) or a file navigation does not cover (neither).
type Doc struct {
	Rel string // slash-separated path relative to Root
	// Library is the library path of an extracted copy: a JDK entry name,
	// or a dependency's "<key>!a/b/C.java" (see package deps).
	Library string
}

// Covered reports whether navigation answers queries from d.
func (d Doc) Covered() bool { return d.Rel != "" || d.Library != "" }

// Target is a declaration or a usage: an absolute file path and the 0-based
// line and byte column of the name there.
type Target struct {
	File         string
	Line, Column int
	// Name is the identifier at the target, used to size its range.
	Name string
}

// Workspace is the index of one folder. Its methods are safe for concurrent
// use.
type Workspace struct {
	opt Options
	ix  *codenav.Index

	mu        sync.Mutex
	stamps    map[string]stamp  // indexed path -> version parsed from disk
	overlays  map[string][]byte // open documents, by relative path
	scannedAt time.Time         // when the last applied scan started
	ready     bool
	scan      *scanRun // the scan in flight, nil when idle
	stale     bool     // a change was reported after scannedAt
	sources   *jdk.Sources
	looked    bool             // the JDK was looked for
	deps      *deps.Set        // the dependency jars, nil until listed
	poms      map[string]stamp // the pom.xml files deps was read from
	depsAt    time.Time        // when deps was read
	lib       *codenav.Library // the JDK and deps, as the index looks names up
}

type stamp struct {
	size int64
	mod  time.Time
}

func (a stamp) same(b stamp) bool { return a.size == b.size && a.mod.Equal(b.mod) }

// scanRun is a scan that queries wait for.
type scanRun struct {
	done chan struct{}
	err  error
}

// New returns a workspace over opt.Root; nothing is read until the first
// query.
func New(opt Options) *Workspace {
	if opt.ListFiles == nil {
		opt.ListFiles = ListFiles
	}
	if opt.Freshness == 0 {
		opt.Freshness = DefaultFreshness
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Workspace{opt: opt, ix: codenav.NewIndex(), overlays: map[string][]byte{}}
}

// Root is the workspace folder.
func (w *Workspace) Root() string { return w.opt.Root }

// DocOf classifies file, an absolute path.
func (w *Workspace) DocOf(file string) Doc {
	if !codenav.Supports(file) {
		return Doc{}
	}
	w.mu.Lock()
	s, ds := w.sources, w.deps
	w.mu.Unlock()
	if s != nil && w.opt.State != "" {
		if name, ok := s.EntryOf(w.opt.State, file); ok {
			return Doc{Library: name}
		}
	}
	if ds != nil && w.opt.State != "" {
		if p, ok := ds.PathOf(w.opt.State, file); ok {
			return Doc{Library: p}
		}
	}
	rel, err := filepath.Rel(w.opt.Root, file)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return Doc{}
	}
	return Doc{Rel: filepath.ToSlash(rel)}
}

// Open records the editor's text of a workspace document, which the index
// then uses instead of the file on disk.
func (w *Workspace) Open(d Doc, text []byte) {
	if d.Rel == "" {
		return
	}
	w.mu.Lock()
	w.overlays[d.Rel] = text
	w.mu.Unlock()
	w.ix.Put(codenav.ParseFile(d.Rel, text))
}

// Close drops the editor's text of d; the copy on disk is indexed again.
func (w *Workspace) Close(d Doc) {
	if d.Rel == "" {
		return
	}
	w.mu.Lock()
	delete(w.overlays, d.Rel)
	delete(w.stamps, d.Rel) // the next scan reads the file again
	w.mu.Unlock()
	full := w.abs(d.Rel)
	if fi, err := os.Stat(full); err == nil && fi.Mode().IsRegular() && fi.Size() <= MaxFileSize {
		if src, err := os.ReadFile(full); err == nil {
			w.ix.Put(codenav.ParseFile(d.Rel, src))
			return
		}
	}
	w.ix.Remove(d.Rel)
}

// Invalidate makes the next query rescan, for a change reported by the
// editor (a watched file was created, changed or deleted).
func (w *Workspace) Invalidate() {
	w.mu.Lock()
	w.stale = true
	w.mu.Unlock()
}

// Definition finds the declaration of the identifier at byte offset off of
// text, the editor's content of d. It reports false when there is no
// identifier there or its declaration is neither in the workspace nor in the
// JDK's sources.
func (w *Workspace) Definition(ctx context.Context, d Doc, text []byte, off int) (Target, bool, error) {
	if !d.Covered() {
		return Target{}, false, nil
	}
	if err := w.ensure(ctx); err != nil {
		return Target{}, false, err
	}
	loc, ok, err := w.resolve(d, text, off)
	if err != nil || !ok {
		return Target{}, false, err
	}
	t, err := w.target(loc)
	if err != nil {
		return Target{}, false, err
	}
	return t, true, nil
}

// References lists the places that refer to the declaration named at byte
// offset off of text, the editor's content of d, in path, line and column
// order; with decl set the declaration itself comes first. Only occurrences
// that navigate back to the declaration count. Usages of a JDK declaration
// are the workspace's; only a local declared in a JDK source has usages
// there. more reports that the list stopped at MaxReferences.
func (w *Workspace) References(ctx context.Context, d Doc, text []byte, off int, decl bool) (out []Target, more bool, err error) {
	if !d.Covered() {
		return nil, false, nil
	}
	if err := w.ensure(ctx); err != nil {
		return nil, false, err
	}
	loc, ok, err := w.resolve(d, text, off)
	if err != nil || !ok {
		return nil, false, err
	}
	names := w.ix.UsageNames(loc, loc.Name)
	if decl {
		t, err := w.target(loc)
		if err != nil {
			return nil, false, err
		}
		out = append(out, t)
	}
	var found []codenav.Location
	if loc.Local {
		// A local is used only in its own file, the one asked about.
		if d.Library != "" {
			found = w.ix.LibraryUsages(d.Library, text, loc, names)
		} else {
			found = w.ix.Usages(d.Rel, text, loc, names)
		}
	} else {
		found, err = w.usages(ctx, d, text, loc, names)
		if err != nil {
			return nil, false, err
		}
	}
	for _, l := range found {
		if len(out) >= MaxReferences {
			return out, true, nil
		}
		t, err := w.target(l)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, false, nil
}

// resolve resolves the identifier at off of text, the content of d. A miss
// caused by a JDK source that could not be read is that error.
func (w *Workspace) resolve(d Doc, text []byte, off int) (codenav.Location, bool, error) {
	var loc codenav.Location
	var ok bool
	if d.Library != "" {
		loc, ok = w.ix.ResolveLibrary(d.Library, text, off)
	} else {
		loc, ok = w.ix.Resolve(d.Rel, text, off)
	}
	if ok {
		return loc, true, nil
	}
	w.mu.Lock()
	lib := w.lib
	w.mu.Unlock()
	if lib != nil {
		if err := lib.Err(); err != nil {
			return loc, false, fmt.Errorf("go to definition: %w", err)
		}
	}
	return loc, false, nil
}

// target turns a codenav location into a file, extracting a JDK source.
func (w *Workspace) target(l codenav.Location) (Target, error) {
	t := Target{Line: l.Line, Column: l.Column, Name: l.Name}
	if !l.Library {
		t.File = w.abs(l.Path)
		return t, nil
	}
	w.mu.Lock()
	s, ds := w.sources, w.deps
	w.mu.Unlock()
	if w.opt.State == "" {
		return Target{}, errors.New("no state directory for library sources")
	}
	if strings.Contains(l.Path, deps.Sep) {
		if ds == nil {
			return Target{}, errors.New("no dependency jars")
		}
		file, err := ds.Extract(w.opt.State, l.Path)
		if err != nil {
			return Target{}, err
		}
		t.File = file
		return t, nil
	}
	if s == nil {
		return Target{}, errors.New("no JDK sources")
	}
	file, err := s.Extract(w.opt.State, l.Path)
	if err != nil {
		return Target{}, err
	}
	t.File = file
	return t, nil
}

func (w *Workspace) abs(rel string) string {
	return filepath.Join(w.opt.Root, filepath.FromSlash(rel))
}

// Text returns the content of a target's file: the editor's text of an open
// workspace document, otherwise the file on disk.
func (w *Workspace) Text(file string) ([]byte, error) {
	if d := w.DocOf(file); d.Rel != "" {
		w.mu.Lock()
		text, ok := w.overlays[d.Rel]
		w.mu.Unlock()
		if ok {
			return text, nil
		}
	}
	return os.ReadFile(file)
}

// usages reads the indexed files and collects the usages of loc in path
// order. The asking document stands in for its copy on disk, and open
// documents for theirs.
func (w *Workspace) usages(ctx context.Context, d Doc, text []byte, loc codenav.Location, names []string) ([]codenav.Location, error) {
	files := w.ix.Paths()
	w.mu.Lock()
	overlays := make(map[string][]byte, len(w.overlays))
	for k, v := range w.overlays {
		overlays[k] = v
	}
	w.mu.Unlock()
	if d.Rel != "" {
		overlays[d.Rel] = text
	}
	per := make([][]codenav.Location, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				p := files[i]
				src, ok := overlays[p]
				if !ok {
					full := w.abs(p)
					fi, err := os.Stat(full)
					if err != nil || !fi.Mode().IsRegular() || fi.Size() > MaxFileSize {
						continue
					}
					if src, err = os.ReadFile(full); err != nil {
						continue
					}
				}
				per[i] = w.ix.Usages(p, src, loc, names)
			}
		}()
	}
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("find references: %w", err)
	}
	var out []codenav.Location
	for _, ls := range per {
		out = append(out, ls...)
	}
	return out, nil
}

// ensure brings the index up to date, joining a scan in flight.
func (w *Workspace) ensure(ctx context.Context) error {
	w.mu.Lock()
	fresh := w.ready && !w.stale && w.opt.Freshness > 0 && time.Since(w.scannedAt) < w.opt.Freshness
	if fresh {
		w.mu.Unlock()
		return nil
	}
	run := w.scan
	if run == nil {
		run = &scanRun{done: make(chan struct{})}
		w.scan = run
		go w.runScan(run)
	}
	w.mu.Unlock()
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runScan scans the folder in the background and applies the result. It
// is not cancelled by the query that started it, which other queries may
// be waiting on too.
func (w *Workspace) runScan(run *scanRun) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	started := time.Now()
	w.mu.Lock()
	old := w.stamps
	oldPoms := w.poms
	depsAt := w.depsAt
	w.stale = false
	lookJDK := w.opt.FindJDK != nil && (!w.looked || w.sources != nil && w.sources.Changed())
	if lookJDK {
		w.looked = true
	}
	w.mu.Unlock()

	// Listing src.zip and the dependency jars takes about as long as a first
	// scan (Viff ADR 0041), so they run side by side with it.
	var found *jdk.Sources
	var jdkErr error
	listed := make(chan struct{})
	if lookJDK {
		go func() {
			defer close(listed)
			if j, ok := w.opt.FindJDK(ctx); ok {
				found, jdkErr = jdk.Open(j)
			}
		}()
	} else {
		close(listed)
	}
	all, err := w.opt.ListFiles(ctx, w.opt.Root)
	if err != nil {
		err = fmt.Errorf("list source files: %w", err)
	}
	poms := w.pomStamps(all)
	lookDeps := err == nil && w.opt.MavenRepo != "" && w.opt.State != "" &&
		(oldPoms == nil || !sameStamps(oldPoms, poms) || time.Since(depsAt) > depsRefresh)
	var depSet *deps.Set
	var depsErr error
	depsDone := make(chan struct{})
	if lookDeps {
		go func() {
			defer close(depsDone)
			depSet, depsErr = w.readDeps(ctx, poms)
		}()
	} else {
		close(depsDone)
	}
	var res scanResult
	if err == nil {
		res, err = w.scanFiles(ctx, all, old)
	}
	<-listed
	<-depsDone

	w.mu.Lock()
	changed := false
	if lookJDK {
		if jdkErr != nil {
			w.opt.Logf("JDK sources unavailable: %v", jdkErr)
		}
		if found != nil || jdkErr == nil {
			w.sources = found
			changed = true
		}
		if w.sources != nil {
			w.opt.Logf("JDK sources: %s (%s)", w.sources.Archive(), w.sources.Version())
		}
	}
	if lookDeps {
		if depsErr != nil {
			w.opt.Logf("dependency jars unavailable: %v", depsErr)
		} else {
			w.deps, w.poms, w.depsAt = depSet, poms, started
			changed = true
		}
	}
	if changed {
		w.lib = w.library()
		w.ix.SetLibrary(w.lib)
	}
	if err == nil {
		for _, p := range res.removed {
			if _, open := w.overlays[p]; !open {
				w.ix.Remove(p)
			}
		}
		for _, f := range res.parsed {
			if _, open := w.overlays[f.Path]; !open {
				w.ix.Put(f)
			}
		}
		w.stamps = res.stamps
		w.scannedAt = started
		w.ready = true
	}
	run.err = err
	w.scan = nil
	w.mu.Unlock()
	close(run.done)
	if len(res.parsed) >= releaseAfter || lookDeps {
		// Indexing a tree allocates several times the index it leaves; the
		// runtime would keep those pages and return them only slowly, so a
		// first scan of a large tree would hold about three times the
		// memory the index needs for minutes. Returning them now takes one
		// collection, after the waiting queries were released.
		debug.FreeOSMemory()
	}
}

// library is the codenav library over the JDK's sources and the dependency
// jars, the JDK first so a jar that bundles JDK classes never shadows them.
// The caller holds w.mu.
func (w *Workspace) library() *codenav.Library {
	var paths []string
	s, ds := w.sources, w.deps
	if s != nil {
		paths = append(paths, s.Names()...)
	}
	if ds != nil {
		paths = append(paths, ds.Paths()...)
	}
	if len(paths) == 0 {
		return nil
	}
	return codenav.NewLibrary(paths, func(p string) ([]byte, error) {
		if strings.Contains(p, deps.Sep) {
			return ds.Read(p)
		}
		return s.Read(p)
	})
}

// pomStamps returns the size and modification time of the folder's
// pom.xml files among all, the folder's files.
func (w *Workspace) pomStamps(all []string) map[string]stamp {
	out := map[string]stamp{}
	for _, p := range all {
		if path.Base(p) != "pom.xml" {
			continue
		}
		if fi, err := os.Stat(w.abs(p)); err == nil && fi.Mode().IsRegular() {
			out[p] = stamp{size: fi.Size(), mod: fi.ModTime()}
		}
	}
	return out
}

func sameStamps(a, b map[string]stamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if o, ok := b[k]; !ok || !o.same(v) {
			return false
		}
	}
	return true
}

// readDeps lists the jars of the Maven dependencies the poms declare.
func (w *Workspace) readDeps(ctx context.Context, poms map[string]stamp) (*deps.Set, error) {
	if len(poms) == 0 {
		return deps.Open(nil, w.opt.Logf), nil
	}
	files := make([]string, 0, len(poms))
	for p := range poms {
		files = append(files, w.abs(p))
	}
	jars, err := deps.MavenJars(ctx, files, w.opt.MavenRepo, w.opt.Logf)
	if err != nil {
		return nil, err
	}
	set := deps.Open(jars, w.opt.Logf)
	w.opt.Logf("dependency jars: %d of %d resolved, %d classes", set.Len(), len(jars), set.Classes())
	return set, nil
}

// scanResult is what changed since the previous scan.
type scanResult struct {
	parsed  []*codenav.File
	removed []string
	stamps  map[string]stamp
}

// scanFiles parses the Java files among all, the folder's files, that are new
// or changed since old. Files that disappeared, became unreadable or grew
// past MaxFileSize are reported as removed.
func (w *Workspace) scanFiles(ctx context.Context, all []string, old map[string]stamp) (scanResult, error) {
	var paths []string
	for _, p := range all {
		if codenav.Supports(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	stamps := make([]stamp, len(paths))
	indexed := make([]bool, len(paths))
	parsed := make([]*codenav.File, len(paths))
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The file's bytes and tokens are scratch once its declarations
			// are read, so each worker reuses one buffer of each.
			var fp codenav.FileParser
			var buf []byte
			for i := range next {
				p := paths[i]
				full := w.abs(p)
				fi, err := os.Stat(full)
				if err != nil || !fi.Mode().IsRegular() || fi.Size() > MaxFileSize {
					continue
				}
				st := stamp{size: fi.Size(), mod: fi.ModTime()}
				if o, ok := old[p]; ok && o.same(st) {
					stamps[i], indexed[i] = st, true
					continue
				}
				src, err := readInto(buf, full)
				if err != nil {
					continue
				}
				buf = src
				stamps[i], indexed[i] = st, true
				parsed[i] = fp.Parse(p, src)
			}
		}()
	}
	for i := range paths {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return scanResult{}, fmt.Errorf("index source files: %w", err)
	}
	res := scanResult{stamps: make(map[string]stamp, len(paths))}
	for i, p := range paths {
		if !indexed[i] {
			continue
		}
		res.stamps[p] = stamps[i]
		if parsed[i] != nil {
			res.parsed = append(res.parsed, parsed[i])
		}
	}
	for p := range old {
		if _, ok := res.stamps[p]; !ok {
			res.removed = append(res.removed, p)
		}
	}
	return res, nil
}

func workers() int { return min(runtime.GOMAXPROCS(0), 8) }

// readInto reads the file at path into buf, growing it when the file does
// not fit, and returns the bytes read. A file that grew past MaxFileSize
// since it was listed is refused.
func readInto(buf []byte, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if len(buf) > MaxFileSize {
			return nil, fmt.Errorf("%s: larger than %d bytes", path, MaxFileSize)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
