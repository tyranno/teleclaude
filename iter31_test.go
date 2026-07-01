package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- PruneOldConversations ----

func TestPruneOldConversations_RemovesOldKeepsRecent(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.AddProject("myapp", dir); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	old, err := s.NewConversation("myapp", "old")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	old.LastActivity = time.Now().UTC().AddDate(0, 0, -40)
	if err := s.UpdateConversation("myapp", old); err != nil {
		t.Fatal(err)
	}
	recent, err := s.NewConversation("myapp", "recent")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	recent.LastActivity = time.Now().UTC().AddDate(0, 0, -1)
	if err := s.UpdateConversation("myapp", recent); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneOldConversations(30)
	if err != nil {
		t.Fatalf("PruneOldConversations: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	if _, ok := s.GetConversation("myapp", old.ID); ok {
		t.Error("old conversation should have been removed")
	}
	if _, ok := s.GetConversation("myapp", recent.ID); !ok {
		t.Error("recent conversation should still exist")
	}
}

func TestPruneOldConversations_SkipsActiveConversation(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.AddProject("myapp", dir); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	c, err := s.NewConversation("myapp", "active-but-old")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	c.LastActivity = time.Now().UTC().AddDate(0, 0, -100)
	if err := s.UpdateConversation("myapp", c); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive("myapp", c.ID); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneOldConversations(30)
	if err != nil {
		t.Fatalf("PruneOldConversations: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (active conversation must survive)", n)
	}
	if _, ok := s.GetConversation("myapp", c.ID); !ok {
		t.Error("active conversation should not have been removed")
	}
}

func TestPruneOldConversations_TTLZeroDisables(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.AddProject("myapp", dir); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	c, err := s.NewConversation("myapp", "ancient")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	c.LastActivity = time.Now().UTC().AddDate(-1, 0, 0)
	if err := s.UpdateConversation("myapp", c); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneOldConversations(0)
	if err != nil {
		t.Fatalf("PruneOldConversations: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (ttlDays=0 disables pruning)", n)
	}
}

// ---- PruneHistory ----

func TestPruneHistory_RemovesOldKeepsRecent(t *testing.T) {
	setHistoryDir(t)
	base, err := historyDir()
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(base, "myapp")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldDate := time.Now().AddDate(0, 0, -40).Format("2006-01-02")
	recentDate := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(projDir, oldDate+".md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, recentDate+".md"), []byte("recent"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := PruneHistory(30)
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(projDir, oldDate+".md")); !os.IsNotExist(err) {
		t.Error("old history file should have been removed")
	}
	if _, err := os.Stat(filepath.Join(projDir, recentDate+".md")); err != nil {
		t.Error("recent history file should still exist")
	}
}

func TestPruneHistory_TTLZeroDisables(t *testing.T) {
	setHistoryDir(t)
	base, err := historyDir()
	if err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(base, "myapp")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldDate := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(projDir, oldDate+".md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := PruneHistory(0)
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (ttlDays=0 disables pruning)", n)
	}
}

func TestPruneHistory_NoHistoryDir(t *testing.T) {
	setHistoryDir(t)
	// historyDir() creates the base dir but no project subdirs exist — should be a no-op.
	n, err := PruneHistory(30)
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0", n)
	}
}
