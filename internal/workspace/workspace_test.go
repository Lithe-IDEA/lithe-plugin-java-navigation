package workspace

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/jdk"
)

// testTimeout bounds every query of these tests.
const testTimeout = 10 * time.Second

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return c
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// at returns the byte offset of the n-th (0-based) occurrence of needle in
// text, plus skip bytes.
func at(t *testing.T, text, needle string, n, skip int) int {
	t.Helper()
	off := -1
	for i := 0; i <= n; i++ {
		j := strings.Index(text[off+1:], needle)
		if j < 0 {
			t.Fatalf("occurrence %d of %q not found", n, needle)
		}
		off += 1 + j
	}
	return off + skip
}

var project = map[string]string{
	"src/main/java/com/acme/model/User.java": `package com.acme.model;

import lombok.Data;

@Data
public class User {
    private String name;
    private int age;

    public User(String name) {
        this.name = name;
    }
}
`,
	"src/main/java/com/acme/service/UserService.java": `package com.acme.service;

import com.acme.model.User;

public class UserService {
    public User create(String name) {
        User user = new User(name);
        System.out.println(user.getName());
        return user;
    }
}
`,
	"target/generated/Ignored.java": "package gen; public class User {}\n",
}

func newTestWorkspace(t *testing.T, root string, findJDK func(context.Context) (jdk.JDK, bool)) *Workspace {
	t.Helper()
	return New(Options{
		Root: root, State: filepath.Join(t.TempDir(), "state"), FindJDK: findJDK,
		ListFiles: walkFiles, Freshness: -1, Logf: t.Logf,
	})
}

func TestDefinitionAndReferencesAcrossFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, project)
	w := newTestWorkspace(t, root, nil)
	svc := project["src/main/java/com/acme/service/UserService.java"]
	doc := w.DocOf(filepath.Join(root, "src/main/java/com/acme/service/UserService.java"))
	if doc.Rel == "" {
		t.Fatalf("DocOf = %+v", doc)
	}

	// The type in "User user = new User(name)" goes to the class.
	tgt, ok, err := w.Definition(ctx(t), doc, []byte(svc), at(t, svc, "User user", 0, 1))
	if err != nil || !ok {
		t.Fatalf("definition = %v %v", ok, err)
	}
	if want := filepath.Join(root, "src/main/java/com/acme/model/User.java"); tgt.File != want || tgt.Line != 5 {
		t.Errorf("type target = %+v, want %s line 5", tgt, want)
	}
	// A Lombok getter goes to its field.
	tgt, ok, err = w.Definition(ctx(t), doc, []byte(svc), at(t, svc, "getName", 0, 2))
	if err != nil || !ok || tgt.Line != 6 || tgt.Name != "getName" {
		t.Errorf("getter target = %+v %v %v, want the name field on line 6", tgt, ok, err)
	}
	// A name the project and the JDK do not declare yields nothing.
	if _, ok, _ := w.Definition(ctx(t), doc, []byte(svc), at(t, svc, "println", 0, 1)); ok {
		t.Error("println resolved without a JDK")
	}

	// References of the name field: the getter call, the constructor's assignment.
	user := project["src/main/java/com/acme/model/User.java"]
	udoc := w.DocOf(filepath.Join(root, "src/main/java/com/acme/model/User.java"))
	refs, more, err := w.References(ctx(t), udoc, []byte(user), at(t, user, "name;", 0, 1), true)
	if err != nil || more {
		t.Fatal(err, more)
	}
	var got []string
	for _, r := range refs {
		rel, _ := filepath.Rel(root, r.File)
		got = append(got, filepath.ToSlash(rel)+":"+strconv.Itoa(r.Line)+":"+r.Name)
	}
	want := []string{
		"src/main/java/com/acme/model/User.java:6:name", // the declaration
		"src/main/java/com/acme/model/User.java:10:name",
		"src/main/java/com/acme/service/UserService.java:7:getName",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("references =\n %v\nwant\n %v", got, want)
	}
}

func TestOpenDocumentsShadowTheDisk(t *testing.T) {
	root := t.TempDir()
	write(t, root, project)
	w := newTestWorkspace(t, root, nil)
	userPath := filepath.Join(root, "src/main/java/com/acme/model/User.java")
	udoc := w.DocOf(userPath)
	svcPath := filepath.Join(root, "src/main/java/com/acme/service/UserService.java")
	sdoc := w.DocOf(svcPath)

	// An unsaved method in an open User is found from another file.
	edited := strings.Replace(project["src/main/java/com/acme/model/User.java"], "    private int age;\n", "    private int age;\n    public void rename(String n) {}\n", 1)
	w.Open(udoc, []byte(edited))
	svc := strings.Replace(project["src/main/java/com/acme/service/UserService.java"], "return user;", "user.rename(name);\n        return user;", 1)
	tgt, ok, err := w.Definition(ctx(t), sdoc, []byte(svc), at(t, svc, "rename", 0, 1))
	if err != nil || !ok || tgt.File != userPath || tgt.Line != 8 {
		t.Fatalf("unsaved declaration = %+v %v %v", tgt, ok, err)
	}
	if text, err := w.Text(userPath); err != nil || string(text) != edited {
		t.Errorf("Text of an open document is not the editor's: %v", err)
	}
	// Closing drops the unsaved edit.
	w.Close(udoc)
	if _, ok, _ := w.Definition(ctx(t), sdoc, []byte(svc), at(t, svc, "rename", 0, 1)); ok {
		t.Error("a closed document's unsaved declaration is still found")
	}
}

func TestRescanSeesChangesOnDisk(t *testing.T) {
	root := t.TempDir()
	write(t, root, project)
	w := newTestWorkspace(t, root, nil)
	svcPath := filepath.Join(root, "src/main/java/com/acme/service/UserService.java")
	sdoc := w.DocOf(svcPath)
	src := "package com.acme.service;\nclass Use { void f() { new com.acme.extra.Extra(); } }\n"
	if _, ok, _ := w.Definition(ctx(t), sdoc, []byte(src), at(t, src, "Extra(", 0, 1)); ok {
		t.Fatal("Extra resolved before it exists")
	}
	write(t, root, map[string]string{"src/main/java/com/acme/extra/Extra.java": "package com.acme.extra;\npublic class Extra {}\n"})
	tgt, ok, err := w.Definition(ctx(t), sdoc, []byte(src), at(t, src, "Extra(", 0, 1))
	if err != nil || !ok || filepath.Base(tgt.File) != "Extra.java" {
		t.Errorf("after the file appeared = %+v %v %v", tgt, ok, err)
	}
}

func TestFilesOutsideTheFolderAreNotCovered(t *testing.T) {
	root := t.TempDir()
	w := newTestWorkspace(t, root, nil)
	for _, p := range []string{filepath.Join(filepath.Dir(root), "Other.java"), filepath.Join(root, "notes.txt"), root} {
		if d := w.DocOf(p); d.Covered() {
			t.Errorf("DocOf(%s) = %+v, want not covered", p, d)
		}
	}
}

func TestWalkSkipsBuildOutputAndHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"src/A.java": "", "target/B.java": "", "build/C.java": "", ".idea/D.java": "", "module/out/E.java": "", "module/src/F.java": "",
	})
	files, err := walkFiles(ctx(t), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(files, " "); got != "module/src/F.java src/A.java" {
		t.Errorf("walk = %s", got)
	}
}

// fakeJDK writes a src.zip and returns a FindJDK that reports it.
func fakeJDK(t *testing.T, files map[string]string) func(context.Context) (jdk.JDK, bool) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return func(context.Context) (jdk.JDK, bool) { return jdk.JDK{Version: "17.0.18", Sources: p}, true }
}

func TestDefinitionIntoTheJDKAndBack(t *testing.T) {
	root := t.TempDir()
	write(t, root, project)
	find := fakeJDK(t, map[string]string{
		"java.base/java/lang/String.java": `package java.lang;

public final class String implements CharSequence {
    public int length() {
        int n = 0;
        return n;
    }
}
`,
		"java.base/java/lang/CharSequence.java": "package java.lang;\n\npublic interface CharSequence {\n    int length();\n}\n",
		"java.base/java/lang/Object.java":       "package java.lang;\n\npublic class Object {\n    public native int hashCode();\n}\n",
	})
	w := newTestWorkspace(t, root, find)
	svcPath := filepath.Join(root, "src/main/java/com/acme/service/UserService.java")
	sdoc := w.DocOf(svcPath)
	svc := project["src/main/java/com/acme/service/UserService.java"]

	// String (implicitly imported from java.lang) opens the extracted copy.
	tgt, ok, err := w.Definition(ctx(t), sdoc, []byte(svc), at(t, svc, "String name", 0, 1))
	if err != nil || !ok {
		t.Fatalf("String = %v %v", ok, err)
	}
	if !strings.Contains(filepath.ToSlash(tgt.File), "/jdk-sources/jdk-17.0.18-") || !strings.HasSuffix(filepath.ToSlash(tgt.File), "java.base/java/lang/String.java") || tgt.Line != 2 {
		t.Fatalf("String target = %+v", tgt)
	}
	// The copy is a JDK source, and navigation continues inside it.
	ldoc := w.DocOf(tgt.File)
	if ldoc.Library != "java.base/java/lang/String.java" {
		t.Fatalf("DocOf(copy) = %+v", ldoc)
	}
	lib, _ := os.ReadFile(tgt.File)
	next, ok, err := w.Definition(ctx(t), ldoc, lib, at(t, string(lib), "CharSequence", 0, 1))
	if err != nil || !ok || !strings.HasSuffix(filepath.ToSlash(next.File), "java/lang/CharSequence.java") {
		t.Errorf("CharSequence from the copy = %+v %v %v", next, ok, err)
	}
	// A local in a JDK source has its usages in that file.
	refs, _, err := w.References(ctx(t), ldoc, lib, at(t, string(lib), "n = 0", 0, 0), false)
	if err != nil || len(refs) != 1 || refs[0].Line != 5 || refs[0].File != tgt.File {
		t.Errorf("local references in a JDK source = %+v %v", refs, err)
	}
	// hashCode() is inherited from java.lang.Object.
	src := "package com.acme.service;\nclass H { int f(UserService s) { return s.hashCode(); } }\n"
	hdoc := w.DocOf(filepath.Join(root, "src/main/java/com/acme/service/H.java"))
	h, ok, err := w.Definition(ctx(t), hdoc, []byte(src), at(t, src, "hashCode", 0, 1))
	if err != nil || !ok || !strings.HasSuffix(filepath.ToSlash(h.File), "java/lang/Object.java") {
		t.Errorf("hashCode = %+v %v %v", h, ok, err)
	}
}
