//go:build !windows

package main

// detachConsole is a no-op on non-Windows platforms: the Linux remote deploy
// runs under nohup and has no console to detach.
func detachConsole() {}
