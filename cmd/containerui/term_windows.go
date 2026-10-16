package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a console.
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// readPassword reads a line from the console f with echo switched off. Line
// input stays on, so the console handles editing and Ctrl+C as usual.
func readPassword(f *os.File) ([]byte, error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return nil, err
	}
	mode := old&^windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return nil, err
	}
	defer windows.SetConsoleMode(h, old)
	return readLine(f)
}
