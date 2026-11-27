package jdk

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// JDK describes an installation with navigable standard library declarations.
type JDK struct {
	Home    string // the JDK's home directory
	Version string // JAVA_VERSION from its release file; "" if unknown
	Archive string // src.zip, or the Java 8 runtime's rt.jar
}

// FindJDK looks for declarations in JAVA_HOME, then the system default JDK,
// then the newest usable installation. A Java 8 runtime with rt.jar is usable
// even without sources, so its declarations take precedence over a different
// JDK's APIs.
//
// It reads environment variables, starts at most one short-lived process
// (macOS) and lists a few well-known install directories; it does not search
// the disk.
func FindJDK(ctx context.Context, selected string, logf func(string, ...any)) (JDK, bool) {
	if selected != "" {
		j, ok := jdkAt(selected)
		if !ok {
			logf("selected workspace JDK has no navigable archive: %s", selected)
		}
		return j, ok
	}
	home, _ := os.UserHomeDir()
	return pickJDK(jdkPreferred(ctx), jdkInstalled(runtime.GOOS, home, os.Getenv))
}

// pickJDK returns the first usable preferred home, else the installed
// usable home with the highest version (the lexically first path among
// equals, so the choice is stable).
func pickJDK(preferred, installed []string) (JDK, bool) {
	for _, h := range preferred {
		if j, ok := jdkAt(h); ok {
			return j, true
		}
	}
	var found []JDK
	seen := map[string]bool{}
	for _, h := range installed {
		j, ok := jdkAt(h)
		if !ok || seen[j.Archive] {
			continue
		}
		seen[j.Archive] = true
		found = append(found, j)
	}
	if len(found) == 0 {
		return JDK{}, false
	}
	sort.SliceStable(found, func(a, b int) bool {
		if c := compareJavaVersions(found[a].Version, found[b].Version); c != 0 {
			return c > 0
		}
		return found[a].Home < found[b].Home
	})
	return found[0], true
}

// jdkAt checks the source archive first, then pre-modular runtime layouts.
func jdkAt(home string) (JDK, bool) {
	if home == "" {
		return JDK{}, false
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	for _, rel := range []string{filepath.Join("lib", "src.zip"), "src.zip"} {
		p := filepath.Join(home, rel)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return JDK{Home: home, Version: releaseVersion(home), Archive: p}, true
		}
	}
	if filepath.Base(home) == "jre" {
		// java may live in <jdk>/jre/bin. Prefer the enclosing JDK's
		// archive (and its release metadata) over the embedded runtime.
		if j, ok := jdkAt(filepath.Dir(home)); ok {
			return j, true
		}
	}
	for _, rel := range []string{filepath.Join("jre", "lib", "rt.jar"), filepath.Join("lib", "rt.jar")} {
		p := filepath.Join(home, rel)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return JDK{Home: home, Version: releaseVersion(home), Archive: p}, true
		}
	}
	return JDK{}, false
}

// releaseVersion reads JAVA_VERSION from home's release file.
func releaseVersion(home string) string {
	f, err := os.Open(filepath.Join(home, "release"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "JAVA_VERSION="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// compareJavaVersions orders "1.8.0_492" < "11.0.31" < "17.0.18" < "21";
// an unknown (empty) version sorts first.
func compareJavaVersions(a, b string) int {
	ka, kb := javaVersionKey(a), javaVersionKey(b)
	for i := 0; i < max(len(ka), len(kb)); i++ {
		var x, y int
		if i < len(ka) {
			x = ka[i]
		}
		if i < len(kb) {
			y = kb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// javaVersionKey splits a version into numbers, mapping the "1.x" scheme of
// Java 8 and earlier to x.
func javaVersionKey(v string) []int {
	var out []int
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	if len(out) > 1 && out[0] == 1 {
		out = out[1:]
	}
	return out
}

// jdkPreferred lists the homes the user chose, most explicit first.
func jdkPreferred(ctx context.Context) []string {
	out := []string{os.Getenv("JAVA_HOME")}
	if runtime.GOOS == "darwin" {
		// /usr/bin/java is a stub on macOS; java_home names the default JDK.
		// A GUI application does not see a shell's JAVA_HOME, so this is the
		// usual answer there.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/libexec/java_home")
		if b, err := cmd.Output(); err == nil {
			out = append(out, strings.TrimSpace(string(b)))
		}
		return out
	}
	if java, err := exec.LookPath("java"); err == nil {
		if real, err := filepath.EvalSymlinks(java); err == nil {
			// <home>/bin/java (or <home>/jre/bin/java for JDK 8).
			out = append(out, filepath.Dir(filepath.Dir(real)))
		}
	}
	return out
}

// jdkInstalled lists the JDK homes in the usual install locations of goos:
// the vendor installers', the package managers' and the ones IntelliJ IDEA
// and SDKMAN! download into.
func jdkInstalled(goos, home string, getenv func(string) string) []string {
	var patterns []string
	user := func(rel ...string) {
		if home != "" {
			patterns = append(patterns, filepath.Join(append([]string{home}, rel...)...))
		}
	}
	switch goos {
	case "darwin":
		patterns = append(patterns,
			"/Library/Java/JavaVirtualMachines/*/Contents/Home",
			"/opt/homebrew/opt/openjdk*/libexec/openjdk.jdk/Contents/Home",
			"/usr/local/opt/openjdk*/libexec/openjdk.jdk/Contents/Home")
		user("Library", "Java", "JavaVirtualMachines", "*", "Contents", "Home")
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if pf := getenv(env); pf != "" {
				for _, vendor := range []string{"Java", "Eclipse Adoptium", "Microsoft", "Zulu", "Amazon Corretto", "BellSoft"} {
					patterns = append(patterns, filepath.Join(pf, vendor, "*"))
				}
			}
		}
		user(".jdks", "*")
	default:
		patterns = append(patterns, "/usr/lib/jvm/*", "/usr/java/*", "/opt/java/*")
		user(".jdks", "*")
	}
	user(".sdkman", "candidates", "java", "*")
	var out []string
	for _, p := range patterns {
		m, _ := filepath.Glob(p) // the patterns are well-formed
		out = append(out, m...)
	}
	return out
}
