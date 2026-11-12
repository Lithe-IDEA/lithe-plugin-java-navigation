package workspace

import (
	"bytes"
	"context"
	"io/fs"
	"os/exec"
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
// build output directories.
func ListFiles(ctx context.Context, root string) ([]string, error) {
	if files, ok := gitFiles(ctx, root); ok {
		return files, nil
	}
	return walkFiles(ctx, root)
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
