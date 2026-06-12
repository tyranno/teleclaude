package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- history.go ----

func setupHistoryDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	historyDirOverride = dir
	t.Cleanup(func() { historyDirOverride = "" })
}

func TestWriteHistory_CreatesFile(t *testing.T) {
	setupHistoryDir(t)
	if err := WriteHistory("myproj", "Test Conversation", "hello", "world"); err != nil {
		t.Fatalf("WriteHistory: %v", err)
	}
	dates, err := ListHistoryDates("myproj")
	if err != nil {
		t.Fatalf("ListHistoryDates: %v", err)
	}
	if len(dates) == 0 {
		t.Error("expected at least one date after WriteHistory")
	}
}

func TestWriteHistory_TruncatesLongResponse(t *testing.T) {
	setupHistoryDir(t)
	longResp := strings.Repeat("x", 600)
	if err := WriteHistory("myproj", "T", "q", longResp); err != nil {
		t.Fatalf("WriteHistory: %v", err)
	}
	date := time.Now().Format("2006-01-02")
	content, err := ReadHistory("myproj", date)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if !strings.Contains(content, "...(생략)") {
		t.Error("long response should be truncated with '...(생략)' suffix")
	}
}

func TestReadHistory_MissingFile_ReturnsEmpty(t *testing.T) {
	setupHistoryDir(t)
	content, err := ReadHistory("no-project", "2000-01-01")
	if err != nil {
		t.Fatalf("ReadHistory on missing file should not error: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty string, got %q", content)
	}
}

func TestListHistoryDates_Descending(t *testing.T) {
	setupHistoryDir(t)
	// Write entries for two different dates by manually creating files.
	base, _ := historyDir()
	projDir := filepath.Join(base, "proj")
	_ = os.MkdirAll(projDir, 0o700)
	_ = os.WriteFile(filepath.Join(projDir, "2026-01-01.md"), []byte("a"), 0o600)
	_ = os.WriteFile(filepath.Join(projDir, "2026-06-12.md"), []byte("b"), 0o600)

	dates, err := ListHistoryDates("proj")
	if err != nil {
		t.Fatalf("ListHistoryDates: %v", err)
	}
	if len(dates) < 2 {
		t.Fatalf("expected 2 dates, got %d", len(dates))
	}
	// Descending order: most recent first
	if dates[0] <= dates[1] {
		t.Errorf("dates not in descending order: %v", dates)
	}
}

func TestListHistoryProjects_ReturnsProjectDirs(t *testing.T) {
	setupHistoryDir(t)
	base, _ := historyDir()
	_ = os.MkdirAll(filepath.Join(base, "alpha"), 0o700)
	_ = os.MkdirAll(filepath.Join(base, "beta"), 0o700)

	projects, err := ListHistoryProjects()
	if err != nil {
		t.Fatalf("ListHistoryProjects: %v", err)
	}
	found := map[string]bool{}
	for _, p := range projects {
		found[p] = true
	}
	if !found["alpha"] || !found["beta"] {
		t.Errorf("expected alpha and beta in projects, got %v", projects)
	}
}

func TestWriteHistory_ConcurrentWrites_NoInterleave(t *testing.T) {
	setupHistoryDir(t)
	const workers = 5
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = WriteHistory("myproj", "T", strings.Repeat("q", 50), strings.Repeat("r", 50))
			_ = n
		}(i)
	}
	wg.Wait()

	date := time.Now().Format("2006-01-02")
	content, err := ReadHistory("myproj", date)
	if err != nil {
		t.Fatalf("ReadHistory after concurrent writes: %v", err)
	}
	// Count number of "## " headers — should match number of writes.
	count := strings.Count(content, "## ")
	if count != workers {
		t.Errorf("expected %d history entries, got %d", workers, count)
	}
}

// ---- sanitizeName ----

func TestSanitizeName_ReplacesSlashAndColon(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my/project", "my_project"},
		{"win:path", "win_path"},
		{"a*b?c", "a_b_c"},
		{"normal", "normal"},
		{"", ""},
	}
	for _, tc := range cases {
		got := sanitizeName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- store.go ----

func TestNextConvID_StartsAt1(t *testing.T) {
	convs := map[string]*Conversation{}
	got := nextConvID(convs)
	if got != "1" {
		t.Errorf("nextConvID({}) = %q, want \"1\"", got)
	}
}

func TestNextConvID_IncrementsPastMax(t *testing.T) {
	convs := map[string]*Conversation{
		"3": {ID: "3"},
		"1": {ID: "1"},
	}
	got := nextConvID(convs)
	if got != "4" {
		t.Errorf("nextConvID({1,3}) = %q, want \"4\"", got)
	}
}

func TestSortedConvIDs_Numeric(t *testing.T) {
	convs := map[string]*Conversation{
		"10": {ID: "10"},
		"2":  {ID: "2"},
		"1":  {ID: "1"},
	}
	ids := sortedConvIDs(convs)
	want := []string{"1", "2", "10"}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("sortedConvIDs[%d] = %q, want %q", i, ids[i], w)
		}
	}
}

func TestFileStore_AddRemoveProject(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.json")
	s := NewFileStore(storePath)

	// AddProject with a valid directory.
	if err := s.AddProject("p1", dir); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if _, ok := s.GetProject("p1"); !ok {
		t.Error("project should exist after AddProject")
	}

	// RemoveProject
	if err := s.RemoveProject("p1"); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if _, ok := s.GetProject("p1"); ok {
		t.Error("project should not exist after RemoveProject")
	}
}

func TestFileStore_NewConversation_IncrementingIDs(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(filepath.Join(dir, "store.json"))
	_ = s.AddProject("p", dir)

	c1, err := s.NewConversation("p", "first")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	c2, err := s.NewConversation("p", "second")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if c1.ID == c2.ID {
		t.Error("consecutive conversations should have different IDs")
	}
	if c2.ID <= c1.ID {
		t.Errorf("second ID %q should be greater than first %q", c2.ID, c1.ID)
	}
}

func TestFileStore_SetGetActive(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(filepath.Join(dir, "store.json"))
	_ = s.AddProject("p", dir)
	c, _ := s.NewConversation("p", "")

	if err := s.SetActive("p", c.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	a := s.GetActive()
	if a.Project != "p" || a.ConversationID != c.ID {
		t.Errorf("GetActive = %+v, want {p, %s}", a, c.ID)
	}
}

func TestFileStore_RemoveProject_ClearsActive(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(filepath.Join(dir, "store.json"))
	_ = s.AddProject("p", dir)
	c, _ := s.NewConversation("p", "")
	_ = s.SetActive("p", c.ID)

	_ = s.RemoveProject("p")
	a := s.GetActive()
	if a.Project != "" {
		t.Errorf("after removing active project, GetActive should be empty, got %+v", a)
	}
}

func TestFileStore_PersistsAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.json")
	s1 := NewFileStore(storePath)
	_ = s1.AddProject("p", dir)

	// Load in a new store instance.
	s2 := NewFileStore(storePath)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := s2.GetProject("p"); !ok {
		t.Error("project should survive load from disk")
	}
}
