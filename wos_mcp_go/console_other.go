//go:build !windows

package main

import "os"

// isStdinPiped returns true if standard input is a pipe or regular file.
func isStdinPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// detachConsole is a no-op on non-Windows platforms: the Linux remote deploy
// runs under nohup and has no console to detach.
func detachConsole() {}
