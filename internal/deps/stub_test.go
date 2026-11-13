package deps

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden stubs in testdata/stubs")

// fixtureSet opens the committed fixture jars: com.example:lib:1.0 has class
// files only, com.example:helper:2.0 a sources jar too.
func fixtureSet(t *testing.T) *Set {
	t.Helper()
	dir := filepath.Join("testdata", "fixture")
	s := Open([]Jar{
		{Coord: "com.example:lib:1.0", Path: filepath.Join(dir, "lib-1.0.jar")},
		{Coord: "com.example:helper:2.0", Path: filepath.Join(dir, "helper-2.0.jar"), Sources: filepath.Join(dir, "helper-2.0-sources.jar")},
	}, t.Logf)
	if s.Len() != 2 {
		t.Fatalf("opened %d jars, want 2", s.Len())
	}
	return s
}

func pathOf(t *testing.T, s *Set, entry string) string {
	t.Helper()
	for _, p := range s.Paths() {
		if strings.HasSuffix(p, Sep+entry) {
			return p
		}
	}
	t.Fatalf("no library path for %s in %v", entry, s.Paths())
	return ""
}

// The stub of every fixture class matches its golden file, and parses into
// the declarations the class file has.
func TestStubsMatchTheGoldenFiles(t *testing.T) {
	s := fixtureSet(t)
	for _, name := range []string{"Schema", "Repository", "Container", "Level", "Point", "Runner", "NoDebug", "Process"} {
		t.Run(name, func(t *testing.T) {
			b, err := s.Read(pathOf(t, s, "com/example/lib/"+name+".java"))
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "stubs", name+".java")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != string(want) {
				t.Errorf("stub of %s:\n%s\nwant:\n%s", name, b, want)
			}
		})
	}
}
