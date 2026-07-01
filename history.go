package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// historyMu serialises concurrent WriteHistory calls.
// O_APPEND is not atomic for large writes on Windows; this ensures well-formed Markdown.
var historyMu sync.Mutex

// historyDirOverride can be set in tests to redirect history I/O to a temp directory.
var historyDirOverride string

// historyDir returns the history base directory (created if needed).
// Defaults to ~/.teleclaude/history; overridden by historyDirOverride in tests.
func historyDir() (string, error) {
	if historyDirOverride != "" {
		if err := os.MkdirAll(historyDirOverride, 0o700); err != nil {
			return "", err
		}
		return historyDirOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".teleclaude", "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteHistory appends a conversation turn to ~/.teleclaude/history/<project>/<YYYY-MM-DD>.md.
// Response is truncated to 500 characters.
// Uses historyMu to serialise concurrent writes from parallel workers.
func WriteHistory(project, title, prompt, response string) error {
	historyMu.Lock()
	defer historyMu.Unlock()

	base, err := historyDir()
	if err != nil {
		return err
	}
	projectDir := filepath.Join(base, sanitizeName(project))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return err
	}

	now := time.Now()
	filename := filepath.Join(projectDir, now.Format("2006-01-02")+".md")

	// Truncate response to 500 characters
	respRunes := []rune(response)
	respShort := string(respRunes)
	suffix := ""
	if len(respRunes) > 500 {
		respShort = string(respRunes[:500])
		suffix = "\n...(생략)"
	}

	entry := fmt.Sprintf("\n## %s — %s\n\n**요청:** %s\n\n**응답:** %s%s\n\n---\n",
		now.Format("15:04"), title, prompt, respShort, suffix)

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry)
	return err
}

// ReadHistory reads the history file for a specific project and date (YYYY-MM-DD).
// Returns empty string if not found (not an error).
func ReadHistory(project, date string) (string, error) {
	base, err := historyDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, sanitizeName(project), date+".md")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

// ListHistoryDates returns all recorded dates for a project (YYYY-MM-DD, descending).
func ListHistoryDates(project string) ([]string, error) {
	base, err := historyDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, sanitizeName(project))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			dates = append(dates, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	return dates, nil
}

// ListHistoryProjects returns all projects that have recorded history.
func ListHistoryProjects() ([]string, error) {
	base, err := historyDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var projects []string
	for _, e := range entries {
		if e.IsDir() {
			projects = append(projects, e.Name())
		}
	}
	return projects, nil
}

// PruneHistory deletes history/<project>/<YYYY-MM-DD>.md files older than
// ttlDays. ttlDays <= 0 disables pruning (returns 0, nil). Returns the number
// of files removed.
func PruneHistory(ttlDays int) (int, error) {
	if ttlDays <= 0 {
		return 0, nil
	}
	base, err := historyDir()
	if err != nil {
		return 0, err
	}
	projectDirs, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().AddDate(0, 0, -ttlDays)
	removed := 0
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		projDir := filepath.Join(base, pd.Name())
		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			date, perr := time.Parse("2006-01-02", strings.TrimSuffix(f.Name(), ".md"))
			if perr != nil {
				continue
			}
			if date.Before(cutoff) {
				if rerr := os.Remove(filepath.Join(projDir, f.Name())); rerr == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

// sanitizeName replaces characters unsafe for directory names with underscores.
func sanitizeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			sb.WriteRune('_')
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
