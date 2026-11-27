package deps

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// GradleJars reads literal dependency declarations and the default version
// catalog. Gradle scripts are executable code; navigation only reads them.
// Published POMs in the cache supply the transitive dependencies.
func GradleJars(ctx context.Context, scripts []string, catalogPath string, logf func(string, ...any)) ([]Jar, error) {
	home := os.Getenv("GRADLE_USER_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		home = filepath.Join(userHome, ".gradle")
	}
	repo := &localRepo{root: filepath.Join(home, "caches", "modules-2", "files-2.1"), gradle: true, versions: map[string][]string{}}
	m := newMaven(ctx, repo, logf)
	catalog := readGradleCatalog(catalogPath, m.logf)
	var primary, extra []*model
	for _, p := range scripts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if src := readGradleFile(p); src != "" {
			groups := gradleDeclarations(src, catalog)
			base := groups[""]
			if len(base) > 0 {
				primary = append(primary, &model{deps: base})
			}
			delete(groups, "")
			for _, configuration := range slices.Sorted(maps.Keys(groups)) {
				extra = append(extra, &model{deps: groups[configuration]})
			}
		}
	}
	// The workspace uses one ordered index. Keep production compile jars
	// first, then add jars from other configurations without evicting them.
	var jars []Jar
	seen := make(map[string]bool)
	for _, root := range append(primary, extra...) {
		resolved, err := m.resolveGradle(root)
		if err != nil {
			return nil, err
		}
		for _, jar := range resolved {
			if !seen[jar.Path] {
				jars = append(jars, jar)
				seen[jar.Path] = true
			}
		}
	}
	return jars, nil
}

// A higher request can be discovered after the lower version's POM has
// already been walked. Walk again with the winners so dependencies unique
// to an evicted version disappear too.
func (m *maven) resolveGradle(root *model) ([]Jar, error) {
	type selection struct {
		version string
		strict  bool
	}
	better := func(a, b selection) bool {
		if a.strict != b.strict {
			return a.strict
		}
		return compareVersions(a.version, b.version) > 0
	}
	var previous, beforePrevious map[string]selection
	const maxConflictPasses = 32
	for pass := 1; ; pass++ {
		requests := map[string]selection{}
		used := map[string]selection{}
		choose := func(dep pomDep, version string) string {
			key := gaOf(dep)
			want := selection{version, dep.strict}
			old, ok := requests[key]
			if !ok || better(want, old) {
				requests[key] = want
			}
			if _, ok := used[key]; !ok {
				chosen, ok := previous[key]
				if !ok {
					chosen = want
				}
				used[key] = chosen
			}
			return used[key].version
		}
		jars, err := m.resolve([]*model{root}, choose)
		if err != nil || maps.Equal(used, requests) {
			return jars, err
		}
		if pass == maxConflictPasses || maps.Equal(beforePrevious, requests) {
			// Cross-upgrading POMs may remove the requests that selected
			// them. Pin the winners of both walks for one final traversal.
			for key, chosen := range used {
				if better(chosen, requests[key]) {
					requests[key] = chosen
				}
			}
			m.logf("Gradle version requests did not stabilize; indexing the selected cached versions")
			return m.resolve([]*model{root}, func(dep pomDep, version string) string {
				if winner, ok := requests[gaOf(dep)]; ok {
					return winner.version
				}
				return version
			})
		}
		beforePrevious, previous = previous, requests
	}
}

// Gradle accepts both Maven-style open bounds and the reversed-bracket form.
func gradleRangeVersion(selector string, versions []string) string {
	if len(selector) < 3 {
		return ""
	}
	left, right := selector[0], selector[len(selector)-1]
	if !strings.ContainsRune("[(]", rune(left)) || !strings.ContainsRune("])[", rune(right)) {
		return ""
	}
	lower, upper, interval := strings.Cut(selector[1:len(selector)-1], ",")
	if strings.ContainsAny(lower+upper, "[](),") || lower == "" && upper == "" {
		return ""
	}
	if !interval {
		if left != '[' || right != ']' {
			return ""
		}
		upper = lower
	}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		lo, hi := compareVersions(v, lower), compareVersions(v, upper)
		if lower != "" && (lo < 0 || lo == 0 && left != '[') {
			continue
		}
		if upper == "" || hi < 0 || hi == 0 && right == ']' {
			return v
		}
	}
	return ""
}

func readGradleFile(path string) string {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() > maxPOMSize {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPOMSize+1))
	if err != nil || len(b) > maxPOMSize {
		return ""
	}
	return string(b)
}

// Consume comments and quoted strings as whole tokens so neither contributes
// declarations. The parser only inspects the direct body of dependencies {}.
var gradleToken = regexp.MustCompile(`(?s)/\*.*?(?:\*/|$)|//[^\n]*|""".*?(?:"""|$)|'''.*?(?:'''|$)|"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|[a-zA-Z0-9_.$]+|[^\s]`)
var gradleConfiguration = regexp.MustCompile(`^(?:[a-zA-Z0-9_]*(?:Implementation|Api|CompileOnly|RuntimeOnly|AnnotationProcessor)|implementation|api|compileOnly|runtimeOnly|annotationProcessor|compile|runtime|testCompile|testRuntime)$`)

type gradleTokens string

func (s *gradleTokens) next() string {
	for {
		i := gradleToken.FindStringIndex(string(*s))
		if i == nil {
			return ""
		}
		tok := string((*s)[i[0]:i[1]])
		*s = (*s)[i[1]:]
		if !strings.HasPrefix(tok, "//") && !strings.HasPrefix(tok, "/*") {
			return tok
		}
	}
}

func gradleDeclarations(script string, catalog map[string]pomDep) map[string][]pomDep {
	tokens := gradleTokens(script)
	var blocks []string
	out := make(map[string][]pomDep)
	buildscript := 0
	prev := ""
	for tok := tokens.next(); tok != ""; tok = tokens.next() {
		switch tok {
		case "{":
			blocks = append(blocks, prev)
			if prev == "buildscript" {
				buildscript++
			}
		case "}":
			if n := len(blocks); n > 0 {
				if blocks[n-1] == "buildscript" {
					buildscript--
				}
				blocks = blocks[:n-1]
			}
		default:
			inDeps := buildscript == 0 && len(blocks) > 0 && blocks[len(blocks)-1] == "dependencies"
			if inDeps && gradleConfiguration.MatchString(tok) {
				if dep := gradleArgument(tokens, catalog); dep.ArtifactID != "" {
					group := tok
					switch tok {
					case "api", "implementation", "compileOnly", "compile":
						group = ""
					}
					out[group] = append(out[group], dep)
				}
			}
		}
		prev = tok
	}
	return out
}

func gradleArgument(tokens gradleTokens, catalog map[string]pomDep) pomDep {
	arg := tokens.next()
	if arg == "(" {
		arg = tokens.next()
	}
	if tokens.next() == "+" {
		return pomDep{}
	}
	if name, ok := strings.CutPrefix(arg, "libs."); ok {
		return catalog[name]
	}
	if len(arg) >= 2 && (arg[0] == '"' || arg[0] == '\'') && arg[len(arg)-1] == arg[0] {
		return gradleCoordinate(arg[1 : len(arg)-1])
	}
	return pomDep{}
}

func gradleCoordinate(s string) pomDep {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 4 || strings.ContainsAny(s, "$@ \t\n\"'") {
		return pomDep{}
	}
	for _, part := range parts {
		if !safeSegment(part) {
			return pomDep{}
		}
	}
	d := pomDep{GroupID: parts[0], ArtifactID: parts[1]}
	if len(parts) >= 3 {
		d.Version = parts[2]
	}
	if len(parts) == 4 {
		d.Classifier = parts[3]
	}
	return d
}

// Catalog aliases are indexed by the dotted accessor Gradle generates. Only
// aliases used by a build script become roots of the dependency graph.
func readGradleCatalog(path string, logf func(string, ...any)) map[string]pomDep {
	src := readGradleFile(path)
	if src == "" {
		return nil
	}
	var catalog struct {
		Libraries map[string]any
		Versions  map[string]any
	}
	if _, err := toml.Decode(src, &catalog); err != nil {
		logf("skipping Gradle version catalog %s: %v", path, err)
		return nil
	}
	aliases := make(map[string]pomDep, len(catalog.Libraries))
	accessor := strings.NewReplacer("-", ".", "_", ".")
	for alias, lib := range catalog.Libraries {
		var dep pomDep
		switch value := lib.(type) {
		case string:
			dep = gradleCoordinate(value)
		case map[string]any:
			strict := false
			module, ok := value["module"].(string)
			if !ok {
				group, _ := value["group"].(string)
				name, _ := value["name"].(string)
				module = group + ":" + name
			}
			if v, exists := value["version"]; exists {
				if ref, ok := v.(map[string]any); ok {
					if key, ok := ref["ref"].(string); ok {
						v = catalog.Versions[key]
					}
				}
				version, _ := v.(string)
				if rich, ok := v.(map[string]any); ok {
					version, strict = rich["strictly"].(string)
					if !strict {
						version, _ = rich["require"].(string)
					}
				}
				module += ":" + version
			}
			dep = gradleCoordinate(module)
			dep.strict = strict
		}
		aliases[accessor.Replace(alias)] = dep
	}
	return aliases
}
