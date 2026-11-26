package deps

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	root := &model{}
	for _, p := range scripts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if src := readGradleFile(p); src != "" {
			root.deps = append(root.deps, gradleDeclarations(src, catalog)...)
		}
	}
	return m.resolve([]*model{root})
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

func gradleDeclarations(script string, catalog map[string]pomDep) []pomDep {
	tokens := gradleTokens(script)
	var blocks []string
	var out []pomDep
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
					out = append(out, dep)
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
			module, ok := value["module"].(string)
			if !ok {
				group, _ := value["group"].(string)
				name, _ := value["name"].(string)
				module = group + ":" + name
			}
			if v, exists := value["version"]; exists {
				version, _ := v.(string)
				if ref, ok := v.(map[string]any); ok {
					key, _ := ref["ref"].(string)
					version, _ = catalog.Versions[key].(string)
				}
				module += ":" + version
			}
			dep = gradleCoordinate(module)
		}
		aliases[accessor.Replace(alias)] = dep
	}
	return aliases
}
