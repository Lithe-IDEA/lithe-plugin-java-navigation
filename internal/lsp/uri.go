package lsp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// pathOf converts a file URI to an absolute path of this system.
func pathOf(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("not a file URI: %s", uri)
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			p = `\\` + u.Host + filepath.FromSlash(p) // UNC path
			return p, nil
		}
		// file:///C:/x has the path /C:/x.
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
	}
	return filepath.Clean(filepath.FromSlash(p)), nil
}

// uriOf converts an absolute path to a file URI.
func uriOf(path string) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(p, "//") {
			rest := strings.TrimPrefix(p, "//")
			host, tail, _ := strings.Cut(rest, "/")
			return (&url.URL{Scheme: "file", Host: host, Path: "/" + tail}).String()
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
