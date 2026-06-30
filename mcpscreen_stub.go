//go:build !windows

package main

import "fmt"

// RunMCPScreen is the non-Windows stub. Screen control relies on Win32 APIs and
// is only implemented under the windows build tag; here it fails fast so the
// caller (main.go's __mcp-screen branch) logs a clear, OS-specific error.
func RunMCPScreen() error { return fmt.Errorf("screen control is Windows-only") }
