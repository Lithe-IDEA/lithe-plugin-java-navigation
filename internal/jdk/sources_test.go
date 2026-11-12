package jdk

import (
	"archive/zip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeZip writes a JDK 9+ style src.zip with files (entry name ->
// content) and returns its path.
func writeZip(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(dir, "src.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSourcesExtractAndMapBack(t *testing.T) {
	dir := t.TempDir()
	archive := writeZip(t, dir, map[string]string{
		"java.base/java/lang/String.java": "package java.lang;\npublic final class String {}\n",
		"java.base/module-info.java":      "module java.base {}\n",
		"README":                          "not a source",
	})
	s, err := Open(JDK{Version: "17.0.18", Sources: archive})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has("java.base/java/lang/String.java") || s.Has("README") {
		t.Fatalf("listed entries are wrong")
	}
	state := filepath.Join(dir, "state")
	file, err := s.Extract(state, "java.base/java/lang/String.java")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "package java.lang;\npublic final class String {}\n" {
		t.Fatalf("extracted %q, %v", b, err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("extracted copy is writable: %v %v", fi.Mode(), err)
	}
	// Extracting again reuses the copy.
	if again, err := s.Extract(state, "java.base/java/lang/String.java"); err != nil || again != file {
		t.Errorf("second extract = %q, %v", again, err)
	}
	if name, ok := s.EntryOf(state, file); !ok || name != "java.base/java/lang/String.java" {
		t.Errorf("EntryOf = %q, %v", name, ok)
	}
	if _, ok := s.EntryOf(state, filepath.Join(dir, "Other.java")); ok {
		t.Error("a file outside the cache maps to an entry")
	}
	if _, err := s.Extract(state, "README"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("extracting a non-source = %v", err)
	}
}

func TestSourcesNoticeAReplacedArchive(t *testing.T) {
	dir := t.TempDir()
	archive := writeZip(t, dir, map[string]string{"java/lang/Object.java": "package java.lang;\npublic class Object {}\n"})
	s, err := Open(JDK{Sources: archive})
	if err != nil {
		t.Fatal(err)
	}
	before := s.CacheDir(dir)
	if s.Changed() {
		t.Fatal("a fresh listing reports a change")
	}
	writeZip(t, dir, map[string]string{"java/lang/Object.java": "package java.lang;\npublic class Object { int changed; }\n"})
	// Pin a different modification time, whatever the file system's resolution.
	if err := os.Chtimes(archive, time.Now(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !s.Changed() {
		t.Fatal("a replaced archive is not reported")
	}
	if _, err := s.Read("java/lang/Object.java"); !errors.Is(err, ErrChanged) {
		t.Errorf("read of a replaced archive = %v, want ErrChanged", err)
	}
	s2, err := Open(JDK{Sources: archive})
	if err != nil {
		t.Fatal(err)
	}
	if s2.CacheDir(dir) == before {
		t.Error("a replaced archive reuses the old cache directory")
	}
}

func TestValidEntry(t *testing.T) {
	for name, want := range map[string]bool{
		"java.base/java/lang/String.java": true,
		"../escape.java":                  false,
		"a/../../escape.java":             false,
		"/abs.java":                       false,
		`a\b.java`:                        false,
		"C:/x.java":                       false,
		"a//b.java":                       false,
	} {
		if got := validEntry(name); got != want {
			t.Errorf("validEntry(%q) = %v, want %v", name, got, want)
		}
	}
}
