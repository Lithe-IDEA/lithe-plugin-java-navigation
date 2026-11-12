package codenav

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// usagesOf resolves the nth occurrence of needle (plus skip) in path, which
// must be a declaration, and lists its usages across the project as
// "path:line:col" (1-based), sorted.
func usagesOf(t *testing.T, ix *Index, path, needle string, nth, skip int) []string {
	t.Helper()
	src := project[path]
	decl, ok := ix.Resolve(path, []byte(src), offsetOf(t, src, needle, nth, skip))
	if !ok || !decl.Self {
		t.Fatalf("%q in %s is not a declaration: %+v ok=%v", needle, path, decl, ok)
	}
	names := ix.UsageNames(decl, decl.Name)
	var out []string
	for _, p := range ix.Paths() {
		if decl.Local && p != path {
			continue
		}
		for _, u := range ix.Usages(p, []byte(project[p]), decl, names) {
			out = append(out, fmt.Sprintf("%s:%d:%d", u.Path, u.Line+1, u.Column+1))
		}
	}
	sort.Strings(out)
	return out
}

// at is the "path:line:col" of the nth occurrence of needle plus skip.
func at(t *testing.T, path, needle string, nth, skip int) string {
	t.Helper()
	l := declAt(t, path, needle, nth, skip)
	return fmt.Sprintf("%s:%d:%d", l.Path, l.Line+1, l.Column+1)
}

func TestUsages(t *testing.T) {
	const (
		svc     = "src/com/acme/service/UserService.java"
		user    = "src/com/acme/model/User.java"
		strs    = "src/com/acme/util/Strings.java"
		repo    = "src/com/acme/service/UserRepo.java"
		status  = "src/com/acme/model/Status.java"
		secured = "src/com/acme/web/Secured.java"
	)
	cases := []struct {
		name         string
		path, needle string
		nth, skip    int
		want         []string
	}{
		{"method: only the calls that pick this overload", user, "greet(String who)", 0, 0, []string{
			at(t, svc, "greet(\"a\")", 0, 0),
			at(t, svc, "greet(\"q\")", 0, 0),
		}},
		{"field with Lombok accessors: fields reads and the setter", user, "name;", 0, 0, []string{
			at(t, svc, "setName", 0, 0),
			at(t, user, "name = name", 0, 0),
			at(t, user, "name; }\n\n    public <T>", 0, 0),
		}},
		{"local variable: its own file only", svc, "local = ", 0, 0, []string{
			at(t, svc, "local);", 0, 0),
			at(t, svc, "local) +", 0, 0),
		}},
		{"static method through a static import, import left out", strs, "trim(", 0, 0, []string{
			at(t, svc, "trim(local)", 0, 0),
		}},
		{"nested type, including a constructor call", user, "Address {", 0, 0, []string{
			at(t, svc, "Address a", 0, 0),
			at(t, svc, "Address()", 0, 0),
			at(t, user, "Address address", 0, 0),
			at(t, user, "Address copy", 0, 0),
		}},
		{"enum constant", status, "ACTIVE", 0, 0, []string{
			at(t, svc, "ACTIVE:", 0, 0),
		}},
		{"annotation attribute", secured, "action()", 0, 0, []string{
			at(t, svc, "action =", 0, 0),
		}},
		{"no usages", status, "DISABLED", 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := usagesOf(t, newProject(t), c.path, c.needle, c.nth, c.skip)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("usages =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}

	// A type with a declared constructor: "new User(...)" is a usage of the
	// type although it navigates to the constructor; the import of a
	// wildcard package and the declaration itself are not.
	got := usagesOf(t, newProject(t), user, "User extends", 0, 0)
	for _, w := range []string{at(t, svc, "User(\"x\")", 0, 0), at(t, svc, "User user", 0, 0), at(t, repo, "User> findAll", 0, 0)} {
		if !contains(got, w) {
			t.Errorf("usages of User miss %s: %v", w, got)
		}
	}
	if contains(got, at(t, user, "User extends", 0, 0)) {
		t.Errorf("usages of User include its declaration: %v", got)
	}
}

func TestSelfAndLocal(t *testing.T) {
	ix := newProject(t)
	const svc = "src/com/acme/service/UserService.java"
	src := []byte(project[svc])
	for _, c := range []struct {
		needle      string
		self, local bool
	}{
		{"run(", true, false},
		{"local = ", true, true},
		{"user, Status", true, true},
		{"trim(local)", false, false},
		{"local) +", false, true},
	} {
		l, ok := ix.Resolve(svc, src, offsetOf(t, project[svc], c.needle, 0, 0))
		if !ok || l.Self != c.self || l.Local != c.local {
			t.Errorf("%q: %+v ok=%v, want self=%v local=%v", c.needle, l, ok, c.self, c.local)
		}
	}
}

func TestContainsName(t *testing.T) {
	src := []byte("int total = subtotal + total_x; String $a;")
	for _, c := range []struct {
		names []string
		want  bool
	}{
		{[]string{"total"}, true},
		{[]string{"sub"}, false},
		{[]string{"total_"}, false},
		{[]string{"x", "$a"}, true},
		{[]string{""}, false},
	} {
		if got := containsName(src, c.names); got != c.want {
			t.Errorf("containsName(%v) = %v, want %v", c.names, got, c.want)
		}
	}
}

// Usage searches run on worker goroutines while the owner keeps resolving
// (and so replacing files); run with -race.
func TestUsagesConcurrentWithResolve(t *testing.T) {
	ix := newProject(t)
	const user = "src/com/acme/model/User.java"
	decl, ok := ix.Resolve(user, []byte(project[user]), offsetOf(t, project[user], "greet(String who)", 0, 0))
	if !ok {
		t.Fatal("greet not resolved")
	}
	names := ix.UsageNames(decl, decl.Name)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				for _, p := range ix.Paths() {
					ix.Usages(p, []byte(project[p]), decl, names)
				}
			}
		}()
	}
	const svc = "src/com/acme/service/UserService.java"
	for range 200 {
		ix.Resolve(svc, []byte(project[svc]), 10)
	}
	wg.Wait()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
