package runtime

import (
	"fmt"
	"io"
	"regexp"
)

// TerminalOpts configures an interactive shell opened in a container.
type TerminalOpts struct {
	Shell      string // absolute path inside the container, from CheckTerminal
	Cols, Rows uint16 // initial terminal size
}

// Terminal is an interactive process attached to a pseudo terminal. Reads
// and writes carry raw terminal bytes (VT sequences included).
type Terminal interface {
	io.ReadWriteCloser // Close ends the process and releases the terminal
	Resize(cols, rows uint16) error
	// Wait blocks until the process exits and returns its exit code.
	Wait() (exitCode int, err error)
}

var shellPathRe = regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,255}$`)

// ValidateShellPath accepts an absolute path made of safe characters only.
func ValidateShellPath(p string) error {
	if !shellPathRe.MatchString(p) {
		return fmt.Errorf("%w: invalid shell path %q", ErrInvalid, p)
	}
	return nil
}
