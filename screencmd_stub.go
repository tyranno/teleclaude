//go:build !windows

package main

import "fmt"

// screenCommand stub — direct screen control is Windows-only.
func screenCommand(sub string, args []string, presetsPath string) (string, []byte, error) {
	return "", nil, fmt.Errorf("화면 제어는 Windows 전용입니다")
}
