package jdk

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeJDK lays out a JDK home under dir: a release file with version (none
// when empty) and src.zip at rel ("" for none).
func fakeJDK(t *testing.T, dir, version, rel string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		if err := os.WriteFile(filepath.Join(dir, "release"), []byte("IMPLEMENTOR=\"x\"\nJAVA_VERSION=\""+version+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if rel != "" {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("PK"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestPickJDK(t *testing.T) {
	root := t.TempDir()
	jdk8 := fakeJDK(t, filepath.Join(root, "zulu-8"), "1.8.0_492", "src.zip")
	jdk11 := fakeJDK(t, filepath.Join(root, "temurin-11"), "11.0.31", filepath.Join("lib", "src.zip"))
	jdk17 := fakeJDK(t, filepath.Join(root, "openjdk-17"), "17.0.18", filepath.Join("lib", "src.zip"))
	jdk21 := fakeJDK(t, filepath.Join(root, "jdk-21"), "21", "") // no sources
	jre := fakeJDK(t, filepath.Join(root, "jre-only"), "17", "")
	installed := []string{jdk8, jdk11, jdk21, jdk17, jre}

	// Without a preference: the newest JDK that has sources.
	j, ok := pickJDK(nil, installed)
	if !ok || j.Home != jdk17 || j.Version != "17.0.18" || j.Sources != filepath.Join(jdk17, "lib", "src.zip") {
		t.Errorf("newest = %+v ok=%v, want %s", j, ok, jdk17)
	}
	// A preferred JDK with sources wins over a newer one; JDK 8 keeps
	// src.zip in its home.
	j, ok = pickJDK([]string{"", jdk8}, installed)
	if !ok || j.Home != jdk8 || j.Sources != filepath.Join(jdk8, "src.zip") {
		t.Errorf("preferred = %+v ok=%v, want %s", j, ok, jdk8)
	}
	// A preferred JDK without sources falls through to the others.
	j, ok = pickJDK([]string{jdk21, filepath.Join(root, "missing")}, installed)
	if !ok || j.Home != jdk17 {
		t.Errorf("fallback = %+v ok=%v, want %s", j, ok, jdk17)
	}
	// JDK 8's java is in jre/bin, so its home is found one level up.
	if err := os.MkdirAll(filepath.Join(jdk8, "jre"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, ok = pickJDK([]string{filepath.Join(jdk8, "jre")}, nil)
	if !ok || j.Home != jdk8 {
		t.Errorf("jre home = %+v ok=%v, want %s", j, ok, jdk8)
	}
	// A symlinked home (Homebrew's opt/openjdk) is the same JDK once.
	link := filepath.Join(root, "link-17")
	if err := os.Symlink(jdk17, link); err == nil {
		j, ok = pickJDK(nil, []string{link, jdk17})
		if !ok || j.Home != jdk17 {
			t.Errorf("symlink = %+v ok=%v", j, ok)
		}
	}
	// Nothing with sources.
	if j, ok := pickJDK([]string{jdk21}, []string{jre}); ok {
		t.Errorf("found %+v without any sources", j)
	}
}

func TestCompareJavaVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.8.0_492", "11.0.31", -1},
		{"17.0.18", "11.0.31", 1},
		{"21", "21.0.1", -1},
		{"17.0.2", "17.0.10", -1},
		{"", "1.8", -1},
		{"17", "17.0.0", 0},
		{"22-ea", "21.0.5", 1},
	} {
		if got := compareJavaVersions(c.a, c.b); got != c.want {
			t.Errorf("compareJavaVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestJDKInstalledLocations(t *testing.T) {
	home := t.TempDir()
	sdk := filepath.Join(home, ".sdkman", "candidates", "java", "17.0.8-tem")
	idea := filepath.Join(home, ".jdks", "corretto-21")
	mac := filepath.Join(home, "Library", "Java", "JavaVirtualMachines", "openjdk-17.jdk", "Contents", "Home")
	pf := filepath.Join(home, "Program Files")
	win := filepath.Join(pf, "Eclipse Adoptium", "jdk-17.0.8.7-hotspot")
	for _, d := range []string{sdk, idea, mac, win} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := func(k string) string {
		if k == "ProgramFiles" {
			return pf
		}
		return ""
	}
	for _, c := range []struct {
		goos string
		want []string
	}{
		{"darwin", []string{mac, sdk}},
		{"linux", []string{idea, sdk}},
		{"windows", []string{win, idea, sdk}},
	} {
		got := map[string]bool{}
		for _, p := range jdkInstalled(c.goos, home, env) {
			got[p] = true
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: %s not listed in %v", c.goos, w, got)
			}
		}
		if c.goos != "windows" && got[win] || c.goos != "darwin" && got[mac] {
			t.Errorf("%s: listed another platform's location: %v", c.goos, got)
		}
	}
}
