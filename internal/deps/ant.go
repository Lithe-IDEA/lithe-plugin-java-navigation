package deps

import (
	"bytes"
	"encoding/xml"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Read only Ant's declarative path entries. Running a target to compute a
// classpath would execute project code on the first navigation request.
func (t *treeJars) antPath(root, build string) {
	project := filepath.Join(root, filepath.FromSlash(build))
	data, err := readLimited(project, maxPOMSize)
	if err != nil {
		return
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charsetReader
	base := filepath.Dir(project)
	props := antProperties{}
	var entries []antEntry
	var parents []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
		switch e := tok.(type) {
		case xml.StartElement:
			name := e.Name.Local
			switch {
			case len(parents) == 0:
				if name != "project" {
					return
				}
				if dir := antAttr(e, "basedir"); dir != "" {
					base = antAbsolute(base, dir)
				}
				props["basedir"] = base
			case len(parents) == 1 && name == "property":
				props.define(e, root, base, filepath.Dir(project))
			case parents[len(parents)-1] == "path" || parents[len(parents)-1] == "classpath":
				if name == "pathelement" || name == "fileset" {
					var entry antEntry
					if dec.DecodeElement(&entry, &e) != nil {
						return
					}
					entries = append(entries, entry)
					continue
				}
			}
			parents = append(parents, name)
		case xml.EndElement:
			parents = parents[:len(parents)-1]
		}
	}
	for _, e := range entries {
		if len(e.Other) != 0 || len(e.Attrs) != 0 {
			continue // Don't widen an entry that uses an unsupported selector.
		}
		if e.XMLName.Local == "pathelement" {
			if p := antLocalPath(root, base, props.expand(e.Location)); p != "" {
				t.add(p, "")
			}
		} else if dir := antLocalPath(root, base, props.expand(e.Dir)); dir != "" {
			t.antFileSet(dir, e, props)
		}
	}
}

func antAttr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func antAbsolute(base, p string) string {
	p = filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// Only inspect archives inside the workspace, including ignored files.
func antLocalPath(root, base, p string) string {
	if p == "" {
		return ""
	}
	full := antAbsolute(base, p)
	rel, err := filepath.Rel(root, full)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return ""
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	rel, err = filepath.Rel(realRoot, real)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	return full
}

type antProperties map[string]string

// Ant properties are immutable. Recognize simple name=value entries of a
// declared sibling build.properties and top-level value/location properties.
func (p antProperties) define(e xml.StartElement, root, base, project string) {
	if file := p.expand(antAttr(e, "file")); file != "" {
		f := antLocalPath(root, base, file)
		if f != filepath.Join(project, "build.properties") {
			return
		}
		data, _ := readLimited(f, maxPOMSize)
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line[0] == '#' || line[0] == '!' {
				continue
			}
			if i := strings.IndexAny(line, "=:"); i >= 0 {
				name := strings.TrimSpace(line[:i])
				if _, set := p[name]; !set {
					p[name] = strings.TrimSpace(line[i+1:])
				}
			}
		}
		return
	}
	name := antAttr(e, "name")
	if _, set := p[name]; name == "" || set {
		return
	}
	if loc := antAttr(e, "location"); loc != "" {
		if value := p.expand(loc); value != "" {
			p[name] = antAbsolute(base, value)
		}
	} else if value := antAttr(e, "value"); value != "" {
		p[name] = value
	}
}

// Bound expansion for cyclic or repeated references. Unknown properties
// leave the whole entry unresolved.
func (p antProperties) expand(value string) string {
	for i := 0; i < 64; i++ {
		start := strings.Index(value, "${")
		if start < 0 {
			return value
		}
		name, after, ok := strings.Cut(value[start+2:], "}")
		v, known := p[name]
		if !ok || !known || len(value)+len(v) > 64*1024 {
			return ""
		}
		value = value[:start] + v + after
	}
	return ""
}

type antEntry struct {
	XMLName  xml.Name
	ID       string       `xml:"id,attr"`
	Location string       `xml:"location,attr"`
	Dir      string       `xml:"dir,attr"`
	Includes string       `xml:"includes,attr"`
	Excludes string       `xml:"excludes,attr"`
	Include  []antPattern `xml:"include"`
	Exclude  []antPattern `xml:"exclude"`
	Other    []struct{}   `xml:",any"`
	Attrs    []xml.Attr   `xml:",any,attr"`
}

var antWildcards = strings.NewReplacer(`\*`, "[^/]*", `\?`, "[^/]")

type antGlob struct {
	re   *regexp.Regexp
	flat bool // all patterns match direct children only
}

type antPattern struct {
	Name  string     `xml:"name,attr"`
	Attrs []xml.Attr `xml:",any,attr"`
}

// Compile Ant's segment wildcards. A ** segment spans zero or more
// directories; a trailing slash is shorthand for a trailing **.
func antPatterns(list string, nested []antPattern, props antProperties) (antGlob, bool) {
	patterns := strings.FieldsFunc(props.expand(list), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if list != "" && len(patterns) == 0 {
		return antGlob{}, false
	}
	for _, p := range nested {
		name := props.expand(p.Name)
		if name == "" || len(p.Attrs) != 0 {
			return antGlob{}, false
		}
		patterns = append(patterns, name)
	}
	if len(patterns) == 0 {
		return antGlob{}, true
	}
	var alternatives []string
	flat := true
	for _, pat := range patterns {
		pat = strings.ReplaceAll(pat, `\`, "/")
		if strings.Contains(pat, "/") || pat == "**" {
			flat = false
		}
		if strings.HasSuffix(pat, "/") {
			pat += "**"
		}
		var b strings.Builder
		for seg := range strings.SplitSeq(pat, "/") {
			if seg == "**" {
				b.WriteString("([^/]+/)*")
			} else {
				quoted := regexp.QuoteMeta(seg)
				b.WriteString(antWildcards.Replace(quoted))
				b.WriteByte('/')
			}
		}
		alternatives = append(alternatives, b.String())
	}
	re, err := regexp.Compile("^(" + strings.Join(alternatives, "|") + ")$")
	return antGlob{re: re, flat: flat}, err == nil
}

func (t *treeJars) antFileSet(dir string, e antEntry, props antProperties) {
	include, ok := antPatterns(e.Includes, e.Include, props)
	if !ok {
		return
	}
	exclude, ok := antPatterns(e.Excludes, e.Exclude, props)
	if !ok {
		return
	}
	// The dependency limit alone doesn't bound a directory full of non-jars.
	visited := 0
	fs.WalkDir(os.DirFS(dir), ".", func(rel string, d fs.DirEntry, err error) error {
		visited++
		if visited > 50000 || len(t.out) >= maxTreeJars {
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() && rel != "." && (include.flat || d.Name() == ".git" || d.Name() == ".svn" || d.Name() == "CVS") {
			return fs.SkipDir
		}
		if d.Type().IsRegular() && isLibraryJar(d.Name()) {
			if (include.re == nil || include.re.MatchString(rel+"/")) && (exclude.re == nil || !exclude.re.MatchString(rel+"/")) {
				t.add(filepath.Join(dir, filepath.FromSlash(rel)), "")
			}
		}
		return nil
	})
}
