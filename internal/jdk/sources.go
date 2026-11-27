package jdk

import (
	"archive/zip"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/codenav"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/deps"
)

// JDK archive indexing (adapted from Viff's ADR 0041). Only the directory
// and entry offsets are read up front. Each lookup opens the archive once,
// checks its identity and reads only the source or class entries it needs.

// MaxFileSize skips sources too large to have been written by hand.
const MaxFileSize = 2 << 20

// ErrChanged reports an archive that changed since it was listed.
var ErrChanged = errors.New("the JDK sources changed on disk")

// Sources provides Java text from src.zip or signature stubs from rt.jar.
// It never changes after Open, so its methods run on any goroutine.
type Sources struct {
	version string
	archive string
	size    int64
	mod     time.Time
	stubs   bool
	entries map[string]entry // raw archive entries, including member classes for stubs
	names   []string         // navigable .java paths, in archive order
}

// entry locates one stored or deflated file in the archive.
type entry struct {
	off, csize, size int64
	deflate          bool
	crc              uint32
}

// Open lists the Java sources of j's archive.
func Open(j JDK) (*Sources, error) {
	f, err := os.Open(j.Archive)
	if err != nil {
		return nil, fmt.Errorf("open JDK sources: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("open JDK sources: %w", err)
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return nil, fmt.Errorf("read JDK sources: %w", err)
	}
	s := &Sources{version: j.Version, archive: j.Archive, size: fi.Size(), mod: fi.ModTime(), entries: map[string]entry{},
		stubs: filepath.Base(j.Archive) == "rt.jar"}
	names := make([]string, 0, len(zr.File))
	for _, e := range zr.File {
		if e.FileInfo().IsDir() || e.Method != zip.Store && e.Method != zip.Deflate || !validEntry(e.Name) {
			continue
		}
		name := e.Name
		if s.stubs {
			stem, ok := strings.CutSuffix(name, ".class")
			if !ok || strings.HasPrefix(name, "META-INF/") {
				continue
			}
			name = stem + ".java"
		} else if !codenav.Supports(name) {
			continue
		}
		off, err := e.DataOffset()
		if err != nil {
			return nil, fmt.Errorf("read JDK sources: %w", err)
		}
		s.entries[e.Name] = entry{off: off, csize: int64(e.CompressedSize64), size: int64(e.UncompressedSize64),
			deflate: e.Method == zip.Deflate, crc: e.CRC32}
		// A member class is rendered in its top-level class's stub.
		if !s.stubs || !strings.Contains(path.Base(name), "$") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("read JDK library: no declarations in %s", j.Archive)
	}
	s.names = names
	return s, nil
}

// Names returns the listed Java sources' entry names, in archive order, for
// a codenav library that reads them with Read.
func (s *Sources) Names() []string { return s.names }

// Version is the JDK's JAVA_VERSION, "" when unknown.
func (s *Sources) Version() string { return s.version }

// Archive is the absolute path of the listed src.zip or rt.jar.
func (s *Sources) Archive() string { return s.archive }

// UsesStubs reports whether navigation displays signatures instead of source text.
func (s *Sources) UsesStubs() bool { return s.stubs }

// Has reports whether name is a listed Java source.
func (s *Sources) Has(name string) bool {
	if s.stubs {
		stem, ok := strings.CutSuffix(name, ".java")
		if !ok || strings.Contains(path.Base(stem), "$") {
			return false
		}
		name = stem + ".class"
	}
	_, ok := s.entries[name]
	return ok
}

// Changed reports that the archive is gone or is no longer the listed file.
func (s *Sources) Changed() bool {
	fi, err := os.Stat(s.archive)
	return err != nil || fi.Size() != s.size || !fi.ModTime().Equal(s.mod)
}

// Read returns the content of one listed entry.
func (s *Sources) Read(name string) ([]byte, error) {
	if !s.Has(name) {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	f, err := os.Open(s.archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != s.size || !fi.ModTime().Equal(s.mod) {
		return nil, ErrChanged
	}
	if s.stubs {
		read := func(name string) ([]byte, error) { return s.readEntry(f, name, deps.MaxEntrySize) }
		has := func(name string) bool {
			_, ok := s.entries[name]
			return ok
		}
		return deps.ClassStub(filepath.Base(s.archive), strings.TrimSuffix(name, ".java"), read, has)
	}
	return s.readEntry(f, name, MaxFileSize)
}

func (s *Sources) readEntry(f *os.File, name string, limit int64) ([]byte, error) {
	e, ok := s.entries[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if e.size < 0 || e.size > limit {
		return nil, fmt.Errorf("%s: invalid size %d (limit %d)", name, e.size, limit)
	}
	var r io.Reader = io.NewSectionReader(f, e.off, e.csize)
	if e.deflate {
		fr := flate.NewReader(r)
		defer fr.Close()
		r = fr
	}
	b := make([]byte, e.size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if crc32.ChecksumIEEE(b) != e.crc {
		return nil, fmt.Errorf("%s: %w", name, ErrChanged)
	}
	return b, nil
}

// CacheDir is the directory under state that holds the extracted copies of
// these sources. It is named after the archive's identity (path, size and
// modification time), so a replaced JDK never reuses copies of the old one.
func (s *Sources) CacheDir(state string) string {
	id := s.archive + "\x00" + strconv.FormatInt(s.size, 10) + "\x00" + s.mod.UTC().Format(time.RFC3339Nano)
	if s.stubs {
		id += "\x00stub " + deps.StubFormat
	}
	h := sha256.Sum256([]byte(id))
	name := "jdk"
	if s.version != "" {
		name += "-" + sanitize(s.version)
	}
	return filepath.Join(state, "jdk-sources", name+"-"+hex.EncodeToString(h[:6]))
}

// Extract writes a read-only copy of entry name under CacheDir(state), so an
// editor can open it as an ordinary file, and returns its absolute path. An
// existing copy of the same size is reused. The copy is written to a
// temporary file first and renamed into place, so a reader never sees half a
// file.
func (s *Sources) Extract(state, name string) (string, error) {
	if !s.Has(name) || !validEntry(name) {
		return "", fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	dst := filepath.Join(s.CacheDir(state), filepath.FromSlash(name))
	if fi, err := os.Stat(dst); err == nil && fi.Mode().IsRegular() && (s.stubs || fi.Size() == s.entries[name].size) {
		return dst, nil
	}
	b, err := s.Read(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("extract JDK source: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".extract-*")
	if err != nil {
		return "", fmt.Errorf("extract JDK source: %w", err)
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("extract JDK source: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("extract JDK source: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		// Another request may have extracted the same entry; Windows refuses
		// to replace a read-only file.
		if fi, serr := os.Stat(dst); serr == nil && fi.Mode().IsRegular() && fi.Size() == int64(len(b)) {
			return dst, nil
		}
		return "", fmt.Errorf("extract JDK source: %w", err)
	}
	return dst, nil
}

// EntryOf maps file, an absolute path, back to the entry it is a copy of,
// when it lies under CacheDir(state).
func (s *Sources) EntryOf(state, file string) (string, bool) {
	rel, err := filepath.Rel(s.CacheDir(state), file)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	name := filepath.ToSlash(rel)
	return name, s.Has(name)
}

// validEntry refuses entry names that would escape the cache directory.
func validEntry(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:"+deps.Sep) {
		return false
	}
	return path.Clean(name) == name && !strings.HasPrefix(name, "../") && name != ".."
}

// sanitize keeps a version usable as part of a directory name.
func sanitize(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, v)
}
