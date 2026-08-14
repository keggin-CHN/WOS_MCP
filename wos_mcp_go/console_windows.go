//go:build windows

package main

import (
	"fmt"
	"syscall"
)

var (
	kernel32dll     = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole = kernel32dll.NewProc("FreeConsole")
)

// detachConsole closes the console window this process was launched with
// (double-click on the .exe) while keeping the process running silently in the
// background. When launched by an MCP client over stdio there is no console
// attached and FreeConsole is a harmless no-op.
func detachConsole() {
	r, _, err := procFreeConsole.Call()
	if r == 0 {
		fmt.Printf("[Main] FreeConsole failed: %v\n", err)
		return
	}
	fmt.Println("[Main] Console detached — running silently in background.")
}
