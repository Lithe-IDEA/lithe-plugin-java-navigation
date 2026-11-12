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
)

// The JDK's class library sources, read from its src.zip (adapted from Viff's
// ADR 0041). Only the archive's directory is read up front; codenav parses a
// class the first time a lookup needs it. The archive is not kept open: each
// read opens it, checks that it is still the file that was listed (a JDK
// update replaces it) and reads one entry.

// MaxFileSize skips sources too large to have been written by hand.
const MaxFileSize = 2 << 20

// ErrChanged reports an archive that changed since it was listed.
var ErrChanged = errors.New("the JDK sources changed on disk")

// Sources is the listed content of a JDK's src.zip. It never changes after
// Open, so its methods run on any goroutine.
type Sources struct {
	version string
	archive string
	size    int64
	mod     time.Time
	entries map[string]entry // by entry name, .java files only
	lib     *codenav.Library
}

// entry locates one stored or deflated file in the archive.
type entry struct {
	off, csize, size int64
	deflate          bool
	crc              uint32
}

// Open lists the Java sources of j's archive.
func Open(j JDK) (*Sources, error) {
	f, err := os.Open(j.Sources)
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
	s := &Sources{version: j.Version, archive: j.Sources, size: fi.Size(), mod: fi.ModTime(), entries: map[string]entry{}}
	names := make([]string, 0, len(zr.File))
	for _, e := range zr.File {
		if !codenav.Supports(e.Name) || e.FileInfo().IsDir() || e.Method != zip.Store && e.Method != zip.Deflate {
			continue
		}
		off, err := e.DataOffset()
		if err != nil {
			return nil, fmt.Errorf("read JDK sources: %w", err)
		}
		s.entries[e.Name] = entry{off: off, csize: int64(e.CompressedSize64), size: int64(e.UncompressedSize64),
			deflate: e.Method == zip.Deflate, crc: e.CRC32}
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("read JDK sources: no Java sources in %s", j.Sources)
	}
	s.lib = codenav.NewLibrary(names, s.Read)
	return s, nil
}

// Library is the codenav library over these sources.
func (s *Sources) Library() *codenav.Library { return s.lib }

// Version is the JDK's JAVA_VERSION, "" when unknown.
func (s *Sources) Version() string { return s.version }

// Archive is the absolute path of the listed src.zip.
func (s *Sources) Archive() string { return s.archive }

// Has reports whether name is a listed Java source.
func (s *Sources) Has(name string) bool {
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
	e, ok := s.entries[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if e.size > MaxFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, MaxFileSize)
	}
	f, err := os.Open(s.archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != s.size || !fi.ModTime().Equal(s.mod) {
		return nil, ErrChanged
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
	h := sha256.Sum256([]byte(s.archive + "\x00" + strconv.FormatInt(s.size, 10) + "\x00" + s.mod.UTC().Format(time.RFC3339Nano)))
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
	if fi, err := os.Stat(dst); err == nil && fi.Mode().IsRegular() && fi.Size() == s.entries[name].size {
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
		if fi, serr := os.Stat(dst); serr == nil && fi.Size() == s.entries[name].size {
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
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, ":") {
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
