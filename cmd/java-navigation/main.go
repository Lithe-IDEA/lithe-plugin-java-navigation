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

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/jdk"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/lsp"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	state := flag.String("state-dir", "", "writable directory for extracted JDK sources (the host's ${stateDir})")
	printVersion := flag.Bool("version", false, "print the version and exit")
	noJDK := flag.Bool("no-jdk", false, "do not navigate into the JDK's sources")
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
	if *state == "" {
		logger.Printf("no --state-dir: navigation into the JDK is off")
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
