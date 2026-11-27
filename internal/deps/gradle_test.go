package deps

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGradleJarsFollowOnlyDeclaredDependencies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GRADLE_USER_HOME", home)
	cache := filepath.Join(home, "caches", "modules-2", "files-2.1", "org.example")
	put := func(artifact, version, suffix, content string) string {
		t.Helper()
		dir := filepath.Join(cache, artifact, version, "hash-"+suffix)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, artifact+"-"+version+suffix)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	put("starter", "1.9", ".jar", "")
	starter := put("starter", "1.10", ".jar", "")
	put("starter", "1.10", ".pom", `<project><dependencies><dependency>
		<groupId>org.example</groupId><artifactId>nested</artifactId><version>2</version>
	</dependency></dependencies></project>`)
	nested := put("nested", "2", ".jar", "")
	put("nested", "1", ".jar", "")
	put("nested", "1", ".pom", `<project><dependencies><dependency>
		<groupId>org.example</groupId><artifactId>evicted-child</artifactId><version>1</version>
	</dependency></dependencies></project>`)
	put("evicted-child", "1", ".jar", "")
	testNested := put("nested", "3", ".jar", "")
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
		put(pair[0], "1", ".jar", "")
		put(pair[0], "1", ".pom", `<project><dependencies><dependency><groupId>org.example</groupId><artifactId>`+pair[1]+`</artifactId><version>2</version></dependency></dependencies></project>`)
	}
	a := put("a", "2", ".jar", "")
	b := put("b", "2", ".jar", "")
	sources := put("nested", "2", "-sources.jar", "")
	put("unused", "1", ".jar", "")
	put("pinned", "2", ".jar", "")
	put("ranged", "1.2", ".jar", "")
	ranged := put("ranged", "1.9", ".jar", "")
	put("ranged", "2.0", ".jar", "")
	strict := put("strict", "2", ".jar", "")
	put("strict", "3", ".jar", "")
	catalog := filepath.Join(t.TempDir(), "libs.versions.toml")
	if err := os.WriteFile(catalog, []byte(`[versions]
	pin = { strictly = "2" }
	[libraries]
	strict = { module = "org.example:strict", version.ref = "pin" }
	`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "build.gradle")
	body := `buildscript { dependencies { implementation 'org.example:unused:1' } }
	dependencies {
		implementation 'org.example:starter'
		implementation 'org.example:nested:1'
		api 'org.example:ranged:[1.0,2.0)'
		implementation libs.strict
		testRuntimeOnly 'org.example:nested:3'
		api 'org.example:a:1'
		api 'org.example:b:1'
		testImplementation("org.example:pinned:1")
		// runtimeOnly 'org.example:unused:1'
		/* compileOnly("org.example:unused:1") */
		constraints { api("org.example:unused:1") }
		implementation("org.example:unused:" + version)
	}
	`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := GradleJars(ctx(t), []string{script}, catalog, t.Logf)
	want := []Jar{
		{Coord: "org.example:starter:1.10", Path: starter},
		{Coord: "org.example:nested:2", Path: nested, Sources: sources},
		{Coord: "org.example:ranged:1.9", Path: ranged},
		{Coord: "org.example:strict:2", Path: strict},
		{Coord: "org.example:a:2", Path: a},
		{Coord: "org.example:b:2", Path: b},
		{Coord: "org.example:nested:3", Path: testNested},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GradleJars = %+v, %v; want %+v", got, err, want)
	}
}
