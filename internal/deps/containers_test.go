package deps

import (
	"path/filepath"
	"strings"
	"testing"
)

// repoJar writes group:artifact:version's jar holding entries into a local
// repository, with a POM so the version is listed.
func repoJar(t *testing.T, repo, group, artifact, version string, entries ...string) {
	t.Helper()
	dir := filepath.Join(repo, filepath.FromSlash(strings.ReplaceAll(group, ".", "/")), artifact, version)
	files := map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n"}
	for _, e := range entries {
		files[e] = ""
	}
	writeFile(t, filepath.Join(dir, artifact+"-"+version+".pom"), "<project/>")
	writeJar(t, filepath.Join(dir, artifact+"-"+version+".jar"), files)
}

// servletRepo is a local repository with Servlet, JSP and JUnit releases of
// both packages, as a developer's machine accumulates them.
func servletRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	repoJar(t, repo, "javax.servlet", "javax.servlet-api", "3.1.0", "javax/servlet/http/HttpServlet.class")
	repoJar(t, repo, "javax.servlet", "javax.servlet-api", "4.0.1", "javax/servlet/http/HttpServlet.class")
	repoJar(t, repo, "jakarta.servlet", "jakarta.servlet-api", "4.0.4", "javax/servlet/http/HttpServlet.class")
	repoJar(t, repo, "jakarta.servlet", "jakarta.servlet-api", "6.0.0", "jakarta/servlet/http/HttpServlet.class")
	repoJar(t, repo, "javax.servlet.jsp", "jsp-api", "2.1", "javax/servlet/jsp/JspException.class")
	repoJar(t, repo, "jakarta.servlet.jsp", "jakarta.servlet.jsp-api", "3.1.0", "jakarta/servlet/jsp/JspException.class")
	repoJar(t, repo, "junit", "junit", "3.8.1", "junit/framework/TestCase.class")
	repoJar(t, repo, "junit", "junit", "4.12", "org/junit/Test.class")
	repoJar(t, repo, "junit", "junit", "4.13.2", "org/junit/Test.class")
	repoJar(t, repo, "org.hamcrest", "hamcrest-core", "1.3", "org/hamcrest/Matcher.class")
	return repo
}

func noServers(t *testing.T) {
	t.Helper()
	saved := serverLibDirs
	serverLibDirs = func() []string { return nil }
	t.Cleanup(func() { serverLibDirs = saved })
}

const dynamicWebClasspath = `<?xml version="1.0" encoding="UTF-8"?>
<classpath>
	<classpathentry kind="src" path="src"/>
	<classpathentry kind="con" path="org.eclipse.jdt.launching.JRE_CONTAINER"/>
	<classpathentry kind="con" path="org.eclipse.jst.j2ee.internal.web.container"/>
	<classpathentry kind="con" path="org.eclipse.jst.server.core.container/org.eclipse.jst.server.tomcat.runtimeTarget/Apache Tomcat %s"/>
	<classpathentry kind="con" path="org.eclipse.jdt.junit.JUNIT_CONTAINER/4"/>
	<classpathentry kind="lib" path="WebContent/WEB-INF/lib/commons-io-2.5.jar"/>
	<classpathentry kind="output" path="build/classes"/>
</classpath>`

// An Eclipse dynamic web project gets its server runtime's Servlet and JSP
// APIs and its JUnit container from the local repository, after the jars it
// keeps; a Tomcat 10 project gets Jakarta EE's packages.
func TestEclipseContainersResolveToLocalJars(t *testing.T) {
	noServers(t)
	repo := servletRepo(t)
	for _, c := range []struct{ runtime, want string }{
		{"v7.0", "commons-io-2.5 javax.servlet:javax.servlet-api:4.0.1 javax.servlet.jsp:jsp-api:2.1 junit:junit:4.13.2 org.hamcrest:hamcrest-core:1.3"},
		{"v10.1", "commons-io-2.5 jakarta.servlet:jakarta.servlet-api:6.0.0 jakarta.servlet.jsp:jakarta.servlet.jsp-api:3.1.0 junit:junit:4.13.2 org.hamcrest:hamcrest-core:1.3"},
	} {
		root := t.TempDir()
		files := []string{treeJar(t, root, "WebContent/WEB-INF/lib/commons-io-2.5.jar"), ".classpath", "WebContent/WEB-INF/web.xml"}
		writeFile(t, filepath.Join(root, ".classpath"), strings.Replace(dynamicWebClasspath, "%s", c.runtime, 1))
		writeFile(t, filepath.Join(root, "WebContent/WEB-INF/web.xml"), "<web-app/>")
		if got := coords(TreeJars(root, files, repo)); got != c.want {
			t.Errorf("Tomcat %s:\n got  %s\n want %s", c.runtime, got, c.want)
		}
	}
}

// A Servlet API the tree keeps itself wins over the repository's, and so
// does a JUnit jar in its lib directory.
func TestContainersDoNotDuplicateTheTreesJars(t *testing.T) {
	noServers(t)
	repo := servletRepo(t)
	root := t.TempDir()
	files := []string{
		treeJar(t, root, "WebContent/WEB-INF/lib/commons-io-2.5.jar"),
		treeJar(t, root, "lib/servlet-api.jar"),
		treeJar(t, root, "lib/junit-4.8.jar"),
		".classpath",
	}
	writeFile(t, filepath.Join(root, ".classpath"), strings.Replace(dynamicWebClasspath, "%s", "v7.0", 1))
	if got, want := coords(TreeJars(root, files, repo)), "commons-io-2.5 servlet-api junit-4.8"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A Tomcat installed on the machine provides the APIs before the
// repository does, when its package matches the runtime's.
func TestAnInstalledServerProvidesTheAPIs(t *testing.T) {
	repo := servletRepo(t)
	tomcat9, tomcat10 := t.TempDir(), t.TempDir()
	writeJar(t, filepath.Join(tomcat9, "servlet-api.jar"), map[string]string{"javax/servlet/http/HttpServlet.class": ""})
	writeJar(t, filepath.Join(tomcat9, "jsp-api.jar"), map[string]string{"javax/servlet/jsp/JspException.class": ""})
	writeJar(t, filepath.Join(tomcat10, "servlet-api.jar"), map[string]string{"jakarta/servlet/http/HttpServlet.class": ""})
	saved := serverLibDirs
	serverLibDirs = func() []string { return []string{tomcat10, tomcat9} }
	t.Cleanup(func() { serverLibDirs = saved })

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".classpath"), strings.Replace(dynamicWebClasspath, "%s", "v9.0", 1))
	got := TreeJars(root, []string{".classpath"}, repo)
	var paths []string
	for _, j := range got {
		paths = append(paths, j.Path)
	}
	want := []string{filepath.Join(tomcat9, "servlet-api.jar"), filepath.Join(tomcat9, "jsp-api.jar")}
	if len(paths) < 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("jars = %v, want the Tomcat 9 APIs %v first", paths, want)
	}
}

// A web application with no build file and no .classpath (MyEclipse,
// NetBeans or a plain Tomcat project) gets the APIs its web.xml implies; one
// with a pom.xml declares them itself.
func TestAWebAppWithoutABuildFileGetsTheServletAPI(t *testing.T) {
	noServers(t)
	repo := servletRepo(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "WebRoot/WEB-INF/web.xml"), `<web-app xmlns="https://jakarta.ee/xml/ns/jakartaee" version="6.0"/>`)
	files := []string{"WebRoot/WEB-INF/web.xml", "src/a/A.java"}
	if got, want := coords(TreeJars(root, files, repo)), "jakarta.servlet:jakarta.servlet-api:6.0.0 jakarta.servlet.jsp:jakarta.servlet.jsp-api:3.1.0"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := TreeJars(root, append(files, "pom.xml"), repo); len(got) != 0 {
		t.Errorf("a Maven web app got %s", coords(got))
	}
}

func TestServerRuntimeContainers(t *testing.T) {
	for _, c := range []struct {
		container, ns string
		ok            bool
	}{
		{"org.eclipse.jst.server.core.container/org.eclipse.jst.server.tomcat.runtimeTarget/Apache Tomcat v8.5", javaxServlet, true},
		{"org.eclipse.jst.server.core.container/org.eclipse.jst.server.tomcat.runtimeTarget/Apache Tomcat v10.0", jakartaServlet, true},
		{"org.eclipse.jst.server.core.container/org.eclipse.jst.server.generic.runtimeTarget/WildFly 26", javaxServlet, true},
		{"org.eclipse.jst.server.core.container/org.eclipse.jst.server.generic.runtimeTarget/WildFly 30", jakartaServlet, true},
		{"com.genuitec.eclipse.j2eedt.core.J2EE14_CONTAINER", javaxServlet, true},
		{"melibrary.com.genuitec.eclipse.j2eedt.core.MYECLIPSE_JAVAEE_5_CONTAINER", javaxServlet, true},
		{"org.eclipse.jst.j2ee.internal.web.container", "", false},
		{"org.eclipse.jdt.launching.JRE_CONTAINER", "", false},
		{"org.eclipse.jdt.junit.JUNIT_CONTAINER/4", "", false},
	} {
		if ns, ok := serverRuntime(c.container); ns != c.ns || ok != c.ok {
			t.Errorf("%s = %q %v, want %q %v", c.container, ns, ok, c.ns, c.ok)
		}
	}
}
