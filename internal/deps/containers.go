package deps

import (
	"archive/zip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// An Eclipse .classpath names some libraries by a container rather than by
// their jars ("kind=con"): the server runtime a web project is deployed to,
// which provides the Servlet and JSP APIs, and JUnit. Eclipse resolves them
// to jars of the developer's machine, so they are not in the tree; without
// them HttpServletRequest, @WebServlet or @Test resolve to nothing in the
// very projects that use them most. The containers are resolved here to jars
// this machine already has: an installed Tomcat's lib directory, else the
// local Maven repository. Nothing is downloaded.
//
// A web application without any build file (a MyEclipse, NetBeans or plain
// Tomcat project with a WEB-INF/web.xml) depends on its server's APIs the
// same way, so it gets them too.

const (
	javaxServlet   = "javax/servlet/"
	jakartaServlet = "jakarta/servlet/"
)

// serverLibDirs lists the lib directories of servers installed on this
// machine, the CATALINA_HOME and CATALINA_BASE ones first. A variable, so
// tests replace it.
var serverLibDirs = func() []string {
	var dirs []string
	for _, env := range []string{"CATALINA_HOME", "CATALINA_BASE"} {
		if v := os.Getenv(env); v != "" {
			dirs = append(dirs, filepath.Join(v, "lib"))
		}
	}
	patterns := []string{"/opt/homebrew/opt/tomcat*/libexec/lib", "/usr/local/opt/tomcat*/libexec/lib",
		"/usr/share/tomcat*/lib", "/opt/tomcat*/lib", "/Library/Tomcat/lib"}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		patterns = append(patterns, filepath.Join(pf, "Apache Software Foundation", "Tomcat *", "lib"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		patterns = append(patterns, filepath.Join(home, "apache-tomcat-*", "lib"))
	}
	for _, p := range patterns {
		found, _ := filepath.Glob(p)
		sort.Sort(sort.Reverse(sort.StringSlice(found)))
		dirs = append(dirs, found...)
	}
	return dirs
}

// runtimeMajor finds the major version in a runtime's name, such as the 10
// of "Apache Tomcat v10.1".
var runtimeMajor = regexp.MustCompile(`\d+`)

// serverRuntime reports whether the container path stands for a server
// runtime, and the package its Servlet API lives in.
func serverRuntime(container string) (ns string, ok bool) {
	segments := strings.Split(container, "/")
	switch {
	case segments[0] == "org.eclipse.jst.server.core.container":
		name := segments[len(segments)-1]
		major, _ := strconv.Atoi(runtimeMajor.FindString(name))
		// The release that moved each server to Jakarta EE 9's packages.
		lower := strings.ToLower(name)
		for server, first := range map[string]int{"tomcat": 10, "jetty": 11, "wildfly": 27, "glassfish": 6, "payara": 6, "tomee": 9} {
			if strings.Contains(lower, server) && major >= first {
				return jakartaServlet, true
			}
		}
		return javaxServlet, true
	case strings.HasPrefix(container, "org.eclipse.jst.j2ee."):
		// The web app libraries (WEB-INF/lib, read anyway) and EAR modules.
		return "", false
	}
	upper := strings.ToUpper(container)
	if strings.Contains(upper, "CONTAINER") && (strings.Contains(upper, "J2EE1") || strings.Contains(upper, "JAVAEE_")) {
		// MyEclipse's J2EE 1.4 and Java EE 5 to 7 libraries.
		return javaxServlet, true
	}
	return "", false
}

// container records what a .classpath container stands for; resolveContainers
// adds its jars once the tree's own are listed.
func (t *treeJars) container(container string) {
	if ns, ok := serverRuntime(container); ok {
		if t.servlet == "" {
			t.servlet = ns
		}
		return
	}
	if rest, ok := strings.CutPrefix(container, "org.eclipse.jdt.junit.JUNIT_CONTAINER"); ok && t.junit == "" {
		t.junit = strings.TrimPrefix(rest, "/")
		if t.junit == "" {
			t.junit = "4"
		}
	}
}

// resolveContainers adds the jars of the recorded containers that the tree
// does not keep itself.
func (t *treeJars) resolveContainers(repo string) {
	if t.servlet != "" && !t.keeps(func(name string) bool {
		return strings.Contains(name, "servlet-api") || strings.Contains(name, "servlet.api") ||
			strings.HasPrefix(name, "javax.servlet") || strings.HasPrefix(name, "jakarta.servlet")
	}) {
		t.serverAPIs(t.servlet, repo)
	}
	if t.junit != "" && repo != "" && !t.keeps(func(name string) bool { return strings.Contains(name, "junit") }) {
		t.addJUnit(t.junit, repo)
	}
}

// keeps reports whether a jar already listed has a lower-case file name
// that matches.
func (t *treeJars) keeps(match func(string) bool) bool {
	for _, j := range t.out {
		if match(strings.ToLower(filepath.Base(j.Path))) {
			return true
		}
	}
	return false
}

// serverAPIs adds the Servlet and JSP APIs in package ns: from a server
// installed on this machine, else from the local repository.
func (t *treeJars) serverAPIs(ns, repo string) {
	for _, dir := range serverLibDirs() {
		api := filepath.Join(dir, "servlet-api.jar")
		if !jarHas(api, ns+"http/HttpServlet.class") {
			continue
		}
		for _, name := range []string{"servlet-api.jar", "jsp-api.jar", "el-api.jar"} {
			t.add(filepath.Join(dir, name), "")
		}
		return
	}
	if repo == "" {
		return
	}
	r := &localRepo{root: repo, versions: map[string][]string{}}
	servlet := [][2]string{{"jakarta.servlet", "jakarta.servlet-api"}}
	jsp := [][2]string{{"jakarta.servlet.jsp", "jakarta.servlet.jsp-api"}}
	if ns == javaxServlet {
		// The javax artifacts first; jakarta's 4.x releases are a last resort.
		servlet = append([][2]string{{"javax.servlet", "javax.servlet-api"}, {"javax.servlet", "servlet-api"}}, servlet...)
		jsp = append([][2]string{{"javax.servlet.jsp", "javax.servlet.jsp-api"}, {"javax.servlet.jsp", "jsp-api"}}, jsp...)
	}
	t.newestHaving(r, servlet, ns+"http/HttpServlet.class")
	t.newestHaving(r, jsp, ns+"jsp/JspException.class")
}

// newestHaving adds the newest jar of the first artifact, in order, that
// holds entry: jakarta.servlet-api 4 still declares javax.servlet, so the
// package is read from the jar rather than assumed from its name.
func (t *treeJars) newestHaving(r *localRepo, artifacts [][2]string, entry string) {
	for _, a := range artifacts {
		versions := r.list(a[0], a[1])
		for i := len(versions) - 1; i >= 0; i-- {
			if t.addRepoJar(r, a[0], a[1], versions[i], entry) {
				return
			}
		}
	}
}

// addJUnit adds the JUnit of a JUNIT_CONTAINER/<version> entry from the
// local repository.
func (t *treeJars) addJUnit(version, repo string) {
	r := &localRepo{root: repo, versions: map[string][]string{}}
	switch version {
	case "5":
		t.addNewest(r, "org.junit.jupiter", "junit-jupiter-api", "5.")
	case "3":
		t.addNewest(r, "junit", "junit", "3.")
	default:
		t.addNewest(r, "junit", "junit", "4.")
		t.addNewest(r, "org.hamcrest", "hamcrest-core", "")
	}
}

// addNewest adds the newest version of group:artifact that starts with
// prefix.
func (t *treeJars) addNewest(r *localRepo, group, artifact, prefix string) {
	versions := r.list(group, artifact)
	for i := len(versions) - 1; i >= 0; i-- {
		if strings.HasPrefix(versions[i], prefix) && t.addRepoJar(r, group, artifact, versions[i], "") {
			return
		}
	}
}

// addRepoJar adds group:artifact:version's jar, with its sources when they
// were downloaded, when it exists and holds entry (any jar when entry is "").
func (t *treeJars) addRepoJar(r *localRepo, group, artifact, version, entry string) bool {
	jar, ok := r.jar(group, artifact, version, "")
	if !ok || entry != "" && !jarHas(jar, entry) {
		return false
	}
	sources, _ := r.file(group, artifact, version, "sources", "jar")
	return t.addAs(jar, sources, group+":"+artifact+":"+version)
}

// jarHas reports whether the jar at p has the entry name.
func jarHas(p, name string) bool {
	z, err := zip.OpenReader(p)
	if err != nil {
		return false
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

// webAppWithoutBuild returns the Servlet package a web application of the
// tree uses when the tree has a WEB-INF/web.xml and no build file to declare
// its server's APIs: Jakarta EE's when the web.xml names its schema.
func webAppWithoutBuild(root string, files []string) (string, bool) {
	webXML := ""
	for _, f := range files {
		switch path.Base(f) {
		case "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts":
			return "", false
		case "web.xml":
			if webXML == "" && path.Base(path.Dir(f)) == "WEB-INF" {
				webXML = f
			}
		}
	}
	if webXML == "" {
		return "", false
	}
	b, err := readLimited(filepath.Join(root, filepath.FromSlash(webXML)), maxPOMSize)
	if err == nil && strings.Contains(string(b), "jakarta.ee/xml/ns/jakartaee") {
		return jakartaServlet, true
	}
	return javaxServlet, true
}
