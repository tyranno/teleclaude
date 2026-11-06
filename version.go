package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Semantic version. major.minor are manual milestones — bump the constants below
// on meaningful releases. The patch number is the git commit count, injected at
// build time by handleUpdate's -ldflags so every commit advances it
// automatically (v1.1.<count>), giving a human-comparable "newer > older".
const (
	versionMajor = 1
	versionMinor = 2
)

// Injected at build time via -ldflags (see handleUpdate). All empty on a plain
// `go build` dev build → the version renders as vMAJOR.MINOR.dev.
var (
	buildCommitCount = "" // git rev-list --count HEAD at build time, e.g. "42"
	buildCommit      = "" // git short hash at build time, e.g. "3c2f050"
	buildTime        = "" // RFC3339 build timestamp
)

// runningVersion renders the human-comparable version string, e.g. "v1.1.42",
// or "v1.1.dev" for an unstamped dev build.
func runningVersion() string {
	return formatVersion(buildCommitCount)
}

// formatVersion renders v<major>.<minor>.<count>; a blank count becomes "dev".
func formatVersion(count string) string {
	if count == "" {
		count = "dev"
	}
	return fmt.Sprintf("v%d.%d.%s", versionMajor, versionMinor, count)
}

// gitShortCommit returns the short HEAD hash for the repo in srcDir, or "" when
// git is unavailable / not a repo.
func gitShortCommit(srcDir string) string {
	return gitOutput(srcDir, "rev-parse", "--short", "HEAD")
}

// gitCommitCount returns the total commit count reachable from HEAD (the patch
// number), or "" when git is unavailable / not a repo.
func gitCommitCount(srcDir string) string {
	return gitOutput(srcDir, "rev-list", "--count", "HEAD")
}

func gitOutput(srcDir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	full := append([]string{"-C", srcDir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// atoiOr parses s or returns def on failure.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
