//go:build !linux && !windows

package main

import (
	"errors"
	"os"
)

// ContainerUI ships for Linux and Windows only; elsewhere the terminal
// prompts are unavailable and passwords are read from stdin as a pipe.

func isTerminal(*os.File) bool { return false }

func readPassword(*os.File) ([]byte, error) {
	return nil, errors.New("reading a password from the terminal is not supported on this platform")
}
