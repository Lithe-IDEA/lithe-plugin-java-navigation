//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package lithe

import (
	"io"
	"os"
)

// protocolOutput is the stream the protocol is written to: stdout itself.
// Here descriptor 1 cannot be redirected, so plugin code must not print to
// stdout; write to stderr or record diagnostics instead.
func protocolOutput() io.Writer { return os.Stdout }
