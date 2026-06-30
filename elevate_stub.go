//go:build !windows

package main

// Non-Windows: no UIPI integrity barrier, so elevation is a no-op.

func isElevated() bool        { return true }
func relaunchElevated() error { return nil }
