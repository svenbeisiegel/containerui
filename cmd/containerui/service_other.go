//go:build !windows

package main

// start runs ContainerUI. Outside Windows a service manager such as systemd
// runs it as it runs any other program, and stops it with SIGTERM.
func start(run func(stop <-chan struct{}) error) error {
	return run(nil)
}
