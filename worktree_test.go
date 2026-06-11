package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initTestRepo creates a minimal git repo with one commit so worktrees can be created.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	for _, c := range cmds {
		if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
			t.Skipf("git not available or init failed: %v (skipping worktree tests)", err)
		}
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("init"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "init"},
	} {
		if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
			t.Skipf("git commit failed: %v (skipping worktree tests)", err)
		}
	}
	return dir
}

func TestIsGitRepo_True(t *testing.T) {
	dir := initTestRepo(t)
	if !isGitRepo(dir) {
		t.Errorf("isGitRepo(%q) = false, want true", dir)
	}
}

func TestIsGitRepo_False(t *testing.T) {
	dir := t.TempDir() // plain temp dir, no .git
	if isGitRepo(dir) {
		t.Errorf("isGitRepo(%q) = true, want false", dir)
	}
}

func TestCreateRemoveWorktree(t *testing.T) {
	dir := initTestRepo(t)

	wtPath, err := CreateWorktree(dir, "test-abc123")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if wtPath == "" {
		t.Fatal("CreateWorktree returned empty path")
	}

	// Worktree directory must exist.
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}

	// Original README must be visible in the worktree.
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("README.md not in worktree: %v", err)
	}

	// Cleanup.
	RemoveWorktree(dir, wtPath)

	// Directory should be gone after removal.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after removal")
	}
}

func TestCreateWorktree_NonGitDir_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	wtPath, err := CreateWorktree(dir, "x")
	if err != nil {
		t.Fatalf("unexpected error for non-git dir: %v", err)
	}
	if wtPath != "" {
		t.Errorf("expected empty path for non-git dir, got %q", wtPath)
	}
}

func TestRemoveWorktree_EmptyPath_NoOp(t *testing.T) {
	// Should not panic or error.
	RemoveWorktree(t.TempDir(), "")
}
