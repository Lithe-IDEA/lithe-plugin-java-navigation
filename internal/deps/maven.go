package deps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Maven dependencies, read without running Maven: the working tree's
// pom.xml files are read with their parents and imported BOMs, and their
// dependencies are followed through the POMs in the local repository, the
// way Maven mediates them (nearest declaration wins, the root's dependency
// management sets versions, test and provided dependencies and optional ones
// are not transitive, exclusions apply). Only what the local repository
// already holds is used: nothing is downloaded, and a dependency whose jar
// was never fetched is left out. Profiles, plugins and settings mirrors are
// not read; a version that names an unknown property or a range resolves to
// the highest version in the local repository.

const (
	// maxArtifacts bounds the dependencies of one working tree.
	maxArtifacts = 4000
	// maxPOMs bounds the POMs read for one resolution.
	maxPOMs = 8000
	// maxParents bounds a parent chain.
	maxParents = 32
	// maxPOMSize skips POM files too large to be hand-written.
	maxPOMSize = 4 << 20
)

// Jar is a dependency archive in the local repository.
type Jar struct {
	Coord   string // "group:artifact:version", with ":classifier" when there is one
	Path    string // the jar
	Sources string // the jar's -sources.jar next to it, "" when it was not downloaded
}

type pomDep struct {
	strict     bool   // an exact or bounded Gradle strictly selector
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Type       string `xml:"type"`
	Classifier string `xml:"classifier"`
	Scope      string `xml:"scope"`
	Optional   string `xml:"optional"`
	Exclusions []struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
	} `xml:"exclusions>exclusion"`
}

type pomXML struct {
	Parent struct {
		GroupID      string  `xml:"groupId"`
		ArtifactID   string  `xml:"artifactId"`
		Version      string  `xml:"version"`
		RelativePath *string `xml:"relativePath"`
	} `xml:"parent"`
	GroupID      string     `xml:"groupId"`
	ArtifactID   string     `xml:"artifactId"`
	Version      string     `xml:"version"`
	Packaging    string     `xml:"packaging"`
	Properties   properties `xml:"properties"`
	Managed      []pomDep   `xml:"dependencyManagement>dependencies>dependency"`
	Dependencies []pomDep   `xml:"dependencies>dependency"`
}

type properties map[string]string

func (p *properties) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	*p = properties{}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var v string
			if err := d.DecodeElement(&v, &t); err != nil {
				return err
			}
			(*p)[t.Name.Local] = strings.TrimSpace(v)
		case xml.EndElement:
			return nil
		}
	}
}

// model is a POM with its parents, imports and interpolation applied.
type model struct {
	group, artifact, version string
	packaging                string
	props                    map[string]string
	managed                  map[string]pomDep // by depKey
	deps                     []pomDep
}

func depKey(d pomDep) string {
	t := d.Type
	if t == "" {
		t = "jar"
	}
	return d.GroupID + ":" + d.ArtifactID + ":" + t + ":" + d.Classifier
}

func gaOf(d pomDep) string { return d.GroupID + ":" + d.ArtifactID }

// maven resolves one working tree.
type maven struct {
	ctx      context.Context
	repo     *localRepo
	raw      map[string]*pomXML // parsed POM files by path; nil marks a failure
	models   map[string]*model  // effective models by path
	building map[string]bool    // cycle guard
	tree     map[string]string  // workspace POM path by "group:artifact"
	reads    int
	logf     func(string, ...any)
}

// MavenJars returns the dependency jars of the Maven projects whose pom.xml
// files are poms (absolute paths, in the working tree), in classpath order:
// the modules' own dependencies first, then theirs. repo is the local
// repository; a module of the working tree is never one of the jars.
func MavenJars(ctx context.Context, poms []string, repo string, logf func(string, ...any)) ([]Jar, error) {
	m := newMaven(ctx, &localRepo{root: repo, versions: map[string][]string{}}, logf)
	sort.Strings(poms)
	var roots []*model
	for _, p := range poms {
		if mo := m.model(p); mo != nil {
			m.tree[mo.group+":"+mo.artifact] = p
			roots = append(roots, mo)
		}
	}
	return m.resolve(roots, nil)
}

func newMaven(ctx context.Context, repo *localRepo, logf func(string, ...any)) *maven {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &maven{ctx: ctx, repo: repo, raw: map[string]*pomXML{}, models: map[string]*model{},
		building: map[string]bool{}, tree: map[string]string{}, logf: logf}
}

// resolve follows the POMs of the direct dependencies supplied by the build files.
func (m *maven) resolve(roots []*model, chooseGradleVersion func(pomDep, string) string) ([]Jar, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	type item struct {
		dep      pomDep
		excluded map[string]bool
		managed  map[string]pomDep
	}
	var queue []item
	for _, r := range roots {
		for _, d := range r.deps {
			queue = append(queue, item{dep: d, managed: r.managed})
		}
	}
	seen := map[string]bool{}
	var out []Jar
	for len(queue) > 0 && len(out) < maxArtifacts {
		if err := m.ctx.Err(); err != nil {
			return nil, err
		}
		it := queue[0]
		queue = queue[1:]
		d := it.dep
		key := gaOf(d) + ":" + d.Classifier
		if d.GroupID == "" || d.ArtifactID == "" || (chooseGradleVersion == nil && seen[key]) {
			continue
		}
		if _, own := m.tree[gaOf(d)]; own {
			continue // a module of the working tree: its sources are indexed
		}
		version := d.Version
		if md, ok := it.managed[depKey(d)]; ok && md.Version != "" {
			version = md.Version
		}
		version = m.repo.pick(d.GroupID, d.ArtifactID, version)
		if chooseGradleVersion != nil {
			version = chooseGradleVersion(d, version)
			if seen[key] {
				continue
			}
		}
		seen[key] = true
		if version == "" {
			continue
		}
		typ := d.Type
		if typ == "" {
			typ = "jar"
		}
		if typ != "pom" && typ != "test-jar" {
			if jar, ok := m.repo.jar(d.GroupID, d.ArtifactID, version, d.Classifier); ok {
				coord := d.GroupID + ":" + d.ArtifactID + ":" + version
				if d.Classifier != "" {
					coord += ":" + d.Classifier
				}
				j := Jar{Coord: coord, Path: jar}
				if d.Classifier == "" {
					j.Sources, _ = m.repo.file(d.GroupID, d.ArtifactID, version, "sources", "jar")
				}
				out = append(out, j)
			}
		}
		pom, ok := m.repo.file(d.GroupID, d.ArtifactID, version, "", "pom")
		if !ok {
			continue
		}
		dm := m.model(pom)
		if dm == nil {
			continue
		}
		excluded := map[string]bool{}
		for k := range it.excluded {
			excluded[k] = true
		}
		for _, e := range d.Exclusions {
			excluded[e.GroupID+":"+e.ArtifactID] = true
		}
		for _, td := range dm.deps {
			scope := td.Scope
			if scope == "" {
				if md, ok := dm.managed[depKey(td)]; ok {
					scope = md.Scope
				}
			}
			if scope == "test" || scope == "provided" || scope == "system" || scope == "import" || td.Optional == "true" {
				continue
			}
			if excluded[gaOf(td)] || excluded[td.GroupID+":*"] || excluded["*:*"] {
				continue
			}
			if td.Version == "" {
				if md, ok := dm.managed[depKey(td)]; ok {
					td.Version = md.Version
				}
			}
			queue = append(queue, item{dep: td, excluded: excluded, managed: it.managed})
		}
	}
	return out, nil
}

// readPOM parses the POM at path once.
func (m *maven) readPOM(path string) *pomXML {
	if p, ok := m.raw[path]; ok {
		return p
	}
	m.raw[path] = nil
	if m.reads >= maxPOMs {
		return nil
	}
	m.reads++
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPOMSize {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p pomXML
	d := xml.NewDecoder(bytes.NewReader(b))
	d.CharsetReader = charsetReader
	if err := d.Decode(&p); err != nil {
		m.logf("skipping %s: %v", path, err)
		return nil
	}
	m.raw[path] = &p
	return &p
}

// model returns the effective model of the POM at path, nil when it cannot
// be read.
func (m *maven) model(path string) *model {
	if mo, ok := m.models[path]; ok {
		return mo
	}
	if m.building[path] || len(m.building) > maxParents || m.ctx.Err() != nil {
		return nil
	}
	m.building[path] = true
	defer delete(m.building, path)
	p := m.readPOM(path)
	if p == nil {
		m.models[path] = nil
		return nil
	}
	mo := &model{props: map[string]string{}, managed: map[string]pomDep{}}
	var parent *model
	if p.Parent.ArtifactID != "" {
		parent = m.parentOf(path, p)
	}
	if parent != nil {
		for k, v := range parent.props {
			mo.props[k] = v
		}
		for k, v := range parent.managed {
			mo.managed[k] = v
		}
	}
	for k, v := range p.Properties {
		mo.props[k] = v
	}
	mo.group, mo.artifact, mo.version, mo.packaging = p.GroupID, p.ArtifactID, p.Version, p.Packaging
	if mo.group == "" {
		mo.group = p.Parent.GroupID
	}
	if mo.version == "" {
		mo.version = p.Parent.Version
	}
	builtin := map[string]string{
		"project.groupId": mo.group, "project.artifactId": mo.artifact, "project.version": mo.version,
		"pom.groupId": mo.group, "pom.artifactId": mo.artifact, "pom.version": mo.version,
		"groupId": mo.group, "artifactId": mo.artifact, "version": mo.version,
		"project.parent.groupId": p.Parent.GroupID, "project.parent.version": p.Parent.Version,
		"parent.version": p.Parent.Version, "project.basedir": filepath.Dir(path), "basedir": filepath.Dir(path),
	}
	for k, v := range builtin {
		if _, ok := mo.props[k]; !ok || strings.HasPrefix(k, "project.") || strings.HasPrefix(k, "pom.") {
			mo.props[k] = v
		}
	}
	mo.group = m.interpolate(mo.group, mo.props)
	mo.version = m.interpolate(mo.version, mo.props)
	mo.props["project.version"], mo.props["project.groupId"] = mo.version, mo.group

	// The POM's own management wins over its parent's, and both over what
	// imported BOMs manage, the first import first.
	own := map[string]pomDep{}
	var imports []pomDep
	for _, d := range p.Managed {
		d = m.interpolateDep(d, mo.props)
		if d.Scope == "import" && d.Type == "pom" {
			imports = append(imports, d)
			continue
		}
		own[depKey(d)] = d
	}
	for k, v := range own {
		mo.managed[k] = v
	}
	for _, d := range imports {
		v := m.repo.pick(d.GroupID, d.ArtifactID, d.Version)
		bom, ok := m.repo.file(d.GroupID, d.ArtifactID, v, "", "pom")
		if !ok {
			continue
		}
		if bm := m.model(bom); bm != nil {
			for k, v := range bm.managed {
				if _, ok := mo.managed[k]; !ok {
					mo.managed[k] = v
				}
			}
		}
	}
	byKey := map[string]int{}
	if parent != nil {
		for _, d := range parent.deps {
			byKey[depKey(d)] = len(mo.deps)
			mo.deps = append(mo.deps, d)
		}
	}
	for _, d := range p.Dependencies {
		d = m.interpolateDep(d, mo.props)
		if i, ok := byKey[depKey(d)]; ok {
			mo.deps[i] = d
			continue
		}
		byKey[depKey(d)] = len(mo.deps)
		mo.deps = append(mo.deps, d)
	}
	for i, d := range mo.deps {
		md, ok := mo.managed[depKey(d)]
		if !ok {
			continue
		}
		if d.Version == "" {
			d.Version = md.Version
		}
		if d.Scope == "" {
			d.Scope = md.Scope
		}
		if len(d.Exclusions) == 0 {
			d.Exclusions = md.Exclusions
		}
		mo.deps[i] = d
	}
	m.models[path] = mo
	return mo
}

// parentOf finds the parent of the POM at path: the file its relativePath
// names (../pom.xml by default) when that is the parent, else the working
// tree's POM of those coordinates, else the local repository's.
func (m *maven) parentOf(path string, p *pomXML) *model {
	rel := "../pom.xml"
	if p.Parent.RelativePath != nil {
		rel = strings.TrimSpace(*p.Parent.RelativePath)
	}
	// Gradle's hash directories never contain a source tree's relative parent.
	if rel != "" && !m.repo.gradle {
		cand := filepath.Join(filepath.Dir(path), filepath.FromSlash(rel))
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			cand = filepath.Join(cand, "pom.xml")
		}
		if pp := m.readPOM(cand); pp != nil && pp.ArtifactID == p.Parent.ArtifactID {
			return m.model(cand)
		}
	}
	if wp, ok := m.tree[p.Parent.GroupID+":"+p.Parent.ArtifactID]; ok {
		return m.model(wp)
	}
	v := m.repo.pick(p.Parent.GroupID, p.Parent.ArtifactID, p.Parent.Version)
	if pom, ok := m.repo.file(p.Parent.GroupID, p.Parent.ArtifactID, v, "", "pom"); ok {
		return m.model(pom)
	}
	return nil
}

func (m *maven) interpolateDep(d pomDep, props map[string]string) pomDep {
	d.GroupID = m.interpolate(d.GroupID, props)
	d.ArtifactID = m.interpolate(d.ArtifactID, props)
	d.Version = m.interpolate(d.Version, props)
	d.Type = m.interpolate(d.Type, props)
	d.Classifier = m.interpolate(d.Classifier, props)
	d.Scope = m.interpolate(d.Scope, props)
	d.Optional = m.interpolate(d.Optional, props)
	return d
}

// interpolate replaces ${name} with the property's value, recursively; an
// unknown property stays as written.
func (m *maven) interpolate(s string, props map[string]string) string {
	s = strings.TrimSpace(s)
	for depth := 0; depth < 10 && strings.Contains(s, "${"); depth++ {
		var b strings.Builder
		rest := s
		changed := false
		for {
			i := strings.Index(rest, "${")
			if i < 0 {
				b.WriteString(rest)
				break
			}
			j := strings.IndexByte(rest[i:], '}')
			if j < 0 {
				b.WriteString(rest)
				break
			}
			name := rest[i+2 : i+j]
			b.WriteString(rest[:i])
			if v, ok := props[name]; ok {
				b.WriteString(v)
				changed = true
			} else if v, ok := strings.CutPrefix(name, "env."); ok && os.Getenv(v) != "" {
				b.WriteString(os.Getenv(v))
				changed = true
			} else {
				b.WriteString(rest[i : i+j+1])
			}
			rest = rest[i+j+1:]
		}
		s = b.String()
		if !changed {
			break
		}
	}
	return s
}

// charsetReader decodes the single-byte encodings POMs declare besides
// UTF-8: ISO-8859-1 (Latin-1) and US-ASCII, whose bytes are their code
// points.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "us-ascii", "ascii", "windows-1252", "cp1252":
		return latin1Reader{bufio.NewReader(in)}, nil
	}
	return nil, fmt.Errorf("unsupported encoding %q", label)
}

type latin1Reader struct{ r *bufio.Reader }

// Read decodes bytes as code points, writing at most len(p) bytes of UTF-8;
// an encoded character never spans two reads.
func (l latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n+2 <= len(p) {
		c, err := l.r.ReadByte()
		if err != nil {
			if n > 0 && err == io.EOF {
				return n, nil
			}
			return n, err
		}
		n += utf8.EncodeRune(p[n:], rune(c))
	}
	if n == 0 && len(p) > 0 {
		return 0, io.ErrShortBuffer
	}
	return n, nil
}

// LocalRepository returns the local Maven repository: the localRepository
// of the user's settings.xml when set, else ~/.m2/repository.
func LocalRepository(home string) string {
	def := filepath.Join(home, ".m2", "repository")
	b, err := os.ReadFile(filepath.Join(home, ".m2", "settings.xml"))
	if err != nil {
		return def
	}
	var s struct {
		LocalRepository string `xml:"localRepository"`
	}
	if xml.Unmarshal(b, &s) != nil {
		return def
	}
	r := strings.TrimSpace(s.LocalRepository)
	if r == "" {
		return def
	}
	r = strings.ReplaceAll(r, "${user.home}", home)
	if strings.HasPrefix(r, "~"+string(filepath.Separator)) || strings.HasPrefix(r, "~/") {
		r = filepath.Join(home, r[2:])
	}
	if !filepath.IsAbs(r) {
		return def
	}
	return r
}

// localRepo reads a Maven repository layout.
type localRepo struct {
	root     string
	gradle   bool                // Gradle files-2.1 layout: literal group ID, one hash directory per file
	versions map[string][]string // listed version directories by "group:artifact"
}

func (r *localRepo) dir(group, artifact string) string {
	if r.gradle {
		return filepath.Join(r.root, group, artifact)
	}
	return filepath.Join(r.root, filepath.FromSlash(strings.ReplaceAll(group, ".", "/")), artifact)
}

// file returns group:artifact:version's file with classifier and extension
// when the repository has it.
func (r *localRepo) file(group, artifact, version, classifier, ext string) (string, bool) {
	if !safeSegment(group) || !safeSegment(artifact) || !safeSegment(version) || classifier != "" && !safeSegment(classifier) {
		return "", false
	}
	name := artifact + "-" + version
	if classifier != "" {
		name += "-" + classifier
	}
	dir := filepath.Join(r.dir(group, artifact), version)
	name += "." + ext
	if r.gradle {
		entries, _ := os.ReadDir(dir)
		for _, hash := range entries {
			if hash.IsDir() {
				p := filepath.Join(dir, hash.Name(), name)
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
					return p, true
				}
			}
		}
		return "", false
	}
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

func (r *localRepo) jar(group, artifact, version, classifier string) (string, bool) {
	return r.file(group, artifact, version, classifier, "jar")
}

// pick returns the version to use: version itself when the repository has
// its POM or jar, else the highest version it has (a range, an unresolved
// property or a version that was never downloaded).
func (r *localRepo) pick(group, artifact, version string) string {
	if r.gradle && strings.ContainsAny(version, "[](),") {
		return gradleRangeVersion(version, r.list(group, artifact))
	}
	if version != "" && !strings.ContainsAny(version, "${[(,") {
		if r.gradle {
			return version // look up the declared version, including classifier-only jars
		}
		if _, ok := r.file(group, artifact, version, "", "pom"); ok {
			return version
		}
		if _, ok := r.jar(group, artifact, version, ""); ok {
			return version
		}
	}
	vs := r.list(group, artifact)
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// list returns the versions of group:artifact in the repository, lowest
// first.
func (r *localRepo) list(group, artifact string) []string {
	key := group + ":" + artifact
	if vs, ok := r.versions[key]; ok {
		return vs
	}
	var vs []string
	if safeSegment(group) && safeSegment(artifact) {
		entries, _ := os.ReadDir(r.dir(group, artifact))
		for _, e := range entries {
			if e.IsDir() && safeSegment(e.Name()) {
				if _, ok := r.file(group, artifact, e.Name(), "", "pom"); ok {
					vs = append(vs, e.Name())
				} else if _, ok := r.jar(group, artifact, e.Name(), ""); ok {
					vs = append(vs, e.Name())
				}
			}
		}
	}
	sort.Slice(vs, func(a, b int) bool { return compareVersions(vs[a], vs[b]) < 0 })
	r.versions[key] = vs
	return vs
}

// safeSegment refuses coordinates that would leave the repository.
func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\:`+"\x00")
}
