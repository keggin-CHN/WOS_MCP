//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
)

var (
	kernel32dll     = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole = kernel32dll.NewProc("FreeConsole")
)

// isStdinPiped returns true if standard input is a pipe or regular file
// (e.g. launched by an MCP client like Claude Desktop / IDE over stdio),
// and false if it is connected to an interactive character device (console/terminal).
func isStdinPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// detachConsole closes the console window this process was launched with
// (double-click on the .exe) while keeping the process running silently in the
// background. When launched by an MCP client over stdio pipes, it is skipped
// to avoid invalidating standard input/output handles.
func detachConsole() {
	if isStdinPiped() {
		return
	}
	r, _, err := procFreeConsole.Call()
	if r == 0 {
		log.Printf("[Main] FreeConsole failed: %v\n", err)
		return
	}
	log.Println("[Main] Console detached — running silently in background.")
}
