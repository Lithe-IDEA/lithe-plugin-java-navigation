package workspace

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds listing the files through Git.
const gitTimeout = 30 * time.Second

// skippedDirs are directory names a walk does not enter: build outputs,
// dependency caches and tool state that hold no hand-written sources.
var skippedDirs = map[string]bool{
	"target": true, "build": true, "out": true, "bin": true, "node_modules": true,
}

// ListFiles lists the files under root as slash-separated relative paths.
// Inside a Git working tree it asks Git for the tracked and untracked files,
// leaving out what .gitignore ignores, as Viff does. Elsewhere, or when Git
// is not installed, it walks root, skipping hidden directories and the usual
// build output directories. Either way the sources a build generated into a
// module's output directory are added (see generatedDirs).
func ListFiles(ctx context.Context, root string) ([]string, error) {
	files, ok := gitFiles(ctx, root)
	if !ok {
		var err error
		if files, err = walkFiles(ctx, root); err != nil {
			return nil, err
		}
	}
	return append(files, generatedFiles(ctx, root, files)...), nil
}

// generatedDirs are where Maven (protobuf, annotation processors, OpenAPI)
// and Gradle write generated sources, relative to the directory of a
// module's build file. The project's code compiles against them, as JDT LS
// sees them, but .gitignore leaves them out with the rest of the output.
var generatedDirs = map[string][]string{
	"pom.xml":          {"target/generated-sources", "target/generated-test-sources"},
	"build.gradle":     {"build/generated"},
	"build.gradle.kts": {"build/generated"},
}

// maxGenerated bounds the generated files one listing adds.
const maxGenerated = 50000

// generatedFiles lists the Java files under the generated source directories
// of the modules whose build files are among files, skipping those already
// listed.
func generatedFiles(ctx context.Context, root string, files []string) []string {
	listed := map[string]bool{}
	var dirs []string
	for _, f := range files {
		listed[f] = true
		dir, base := path.Split(f)
		for _, g := range generatedDirs[base] {
			dirs = append(dirs, dir+g)
		}
	}
	var out []string
	for _, d := range dirs {
		full := filepath.Join(root, filepath.FromSlash(d))
		if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
			continue
		}
		filepath.WalkDir(full, func(p string, e fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil || len(out) >= maxGenerated {
				return filepath.SkipDir
			}
			if e.IsDir() || !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".java") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			if r := filepath.ToSlash(rel); !listed[r] {
				listed[r] = true
				out = append(out, r)
			}
			return nil
		})
	}
	return out
}

// gitFiles lists root's files through Git; ok is false when root is not in a
// working tree or Git is unavailable.
func gitFiles(ctx context.Context, root string) ([]string, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-c", "core.quotePath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	// A file with merge conflicts is listed once per stage.
	seen := map[string]bool{}
	var files []string
	for _, b := range bytes.Split(out, []byte{0}) {
		if len(b) > 0 && !seen[string(b)] {
			seen[string(b)] = true
			files = append(files, string(b))
		}
	}
	return files, true
}

// walkFiles lists root's files by walking the directory tree.
func walkFiles(ctx context.Context, root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // an unreadable directory holds nothing we can index
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}
