package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPluginManifestPermissionsConsistent(t *testing.T) {
	path := filepath.Join("..", "..", "io.github.lithe-idea.java-navigation", "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		Permissions     []string `json:"permissions"`
		LanguageServers []struct {
			RequestMethods       []string `json:"requestMethods"`
			ForwardNotifications []string `json:"forwardNotifications"`
		} `json:"languageServers"`
		Backend json.RawMessage `json:"backend"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	has := make(map[string]bool, len(manifest.Permissions))
	for _, perm := range manifest.Permissions {
		if has[perm] {
			t.Errorf("duplicate permission %q in plugin.json", perm)
		}
		has[perm] = true
	}
	if !slices.IsSorted(manifest.Permissions) {
		t.Errorf("permissions %v should be sorted alphabetically", manifest.Permissions)
	}
	if len(manifest.LanguageServers) > 0 && !has["languageServer.launch"] {
		t.Errorf("languageServers requires languageServer.launch permission")
	}
	for _, srv := range manifest.LanguageServers {
		if (len(srv.RequestMethods) > 0 || len(srv.ForwardNotifications) > 0) && !has["languageServer.request"] {
			t.Errorf("requestMethods/forwardNotifications requires languageServer.request permission")
		}
	}
	if len(manifest.Backend) > 0 && string(manifest.Backend) != "null" && !has["backend.launch"] {
		t.Errorf("backend requires backend.launch permission")
	}
	if (has["editor.actions"] || has["editor.providers"]) && !has["documents.read"] {
		t.Errorf("editor.actions/editor.providers requires documents.read permission (Core manifest rule)")
	}
}
