// Command java-navigation is a lightweight Java language server for Lithe. It
// answers go to definition, find references and the document outline from a
// resident index of declarations, without a JDK runtime, a build import or
// JDT LS. It speaks LSP over standard input and output.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/debug"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/deps"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/jdk"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/lsp"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// gcPercent is the garbage collector's target unless GOGC is set. The server
// keeps an index of tens of MiB and every query allocates scratch; the
// default of 100 lets the heap grow to twice the index before collecting.
// Measured on Nacos (1,500 definitions and 20 reference searches, Apple M4):
// 100 ends at a 68 MiB footprint, 50 at 58 MiB, 25 at 47 MiB, with the
// median definition at 0.11, 0.11 and 0.12 ms.
const gcPercent = 25

func main() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(gcPercent)
	}
	state := flag.String("state-dir", "", "writable directory for extracted JDK sources (the host's ${stateDir})")
	printVersion := flag.Bool("version", false, "print the version and exit")
	noJDK := flag.Bool("no-jdk", false, "do not navigate into the JDK's sources")
	noDeps := flag.Bool("no-dependencies", false, "do not navigate into the jars of Maven dependencies")
	flag.Bool("stdio", true, "speak LSP over standard input and output (the only transport)")
	flag.Parse()
	if *printVersion {
		fmt.Println("java-navigation", version)
		return
	}
	// Standard output carries the protocol; diagnostics go to standard error,
	// which the host records in the server's log.
	logger := log.New(os.Stderr, "java-navigation: ", log.LstdFlags|log.Lmsgprefix)
	opt := lsp.Options{Name: "java-navigation", Version: version, State: *state, Logf: logger.Printf}
	if !*noJDK && *state != "" {
		opt.FindJDK = jdk.FindJDK
	}
	if home, err := os.UserHomeDir(); err == nil && !*noDeps && *state != "" {
		opt.MavenRepo = deps.LocalRepository(home)
	}
	if *state == "" {
		logger.Printf("no --state-dir: navigation into the JDK and dependency jars is off")
	}
	err := lsp.New(os.Stdin, os.Stdout, opt).Run()
	switch {
	case errors.Is(err, lsp.ErrExitWithoutShutdown):
		os.Exit(1)
	case err != nil:
		logger.Printf("%v", err)
		os.Exit(1)
	}
}
