package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// isGitRepo reports whether path is the root of a git repository.
func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// CreateWorktree adds a detached git worktree at a temp path derived from id.
// Returns the worktree directory path. Caller must call RemoveWorktree when done.
// Returns ("", nil) — falling back to original path — when the project is not a git repo.
func CreateWorktree(projectPath, id string) (string, error) {
	if !isGitRepo(projectPath) {
		return "", nil // not a git repo — no worktree needed
	}
	wtPath := filepath.Join(os.TempDir(), "teleclaude-wt-"+id)
	// --detach avoids creating/modifying a branch; HEAD is already a valid ref.
	cmd := exec.Command("git", "-C", projectPath, "worktree", "add", "--detach", wtPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add 실패: %w\n%s", err, string(out))
	}
	return wtPath, nil
}

// RemoveWorktree removes a previously created worktree.
// Logs but does not return errors — temp directories are eventually cleaned up by the OS.
func RemoveWorktree(projectPath, worktreePath string) {
	if worktreePath == "" {
		return
	}
	cmd := exec.Command("git", "-C", projectPath, "worktree", "remove", "--force", worktreePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[worktree] remove failed (will GC): %v — %s", err, string(out))
	}
}
