package main

import (
	"os"
	"path/filepath"
	"testing"
)

func tempUserStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "extra_users.json")
}

func TestUserStore_AddContainsRemove(t *testing.T) {
	u := NewUserStore(tempUserStorePath(t))
	if u.Contains(1) {
		t.Fatal("new store should not contain any user")
	}
	if err := u.Add(1); err != nil {
		t.Fatal(err)
	}
	if !u.Contains(1) {
		t.Error("should contain user 1 after Add")
	}
	if err := u.Remove(1); err != nil {
		t.Fatal(err)
	}
	if u.Contains(1) {
		t.Error("should not contain user 1 after Remove")
	}
}

func TestUserStore_Persist(t *testing.T) {
	p := tempUserStorePath(t)
	u1 := NewUserStore(p)
	_ = u1.Add(42)
	_ = u1.Add(100)

	u2 := NewUserStore(p)
	if err := u2.Load(); err != nil {
		t.Fatal(err)
	}
	if !u2.Contains(42) || !u2.Contains(100) {
		t.Error("loaded store should contain persisted user IDs")
	}
}

func TestUserStore_LoadMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nonexistent.json")
	u := NewUserStore(p)
	if err := u.Load(); err != nil {
		t.Errorf("missing file should not error on Load, got: %v", err)
	}
}

func TestUserStore_List_Sorted(t *testing.T) {
	u := NewUserStore(tempUserStorePath(t))
	_ = u.Add(30)
	_ = u.Add(10)
	_ = u.Add(20)
	ids := u.List()
	if len(ids) != 3 || ids[0] != 10 || ids[1] != 20 || ids[2] != 30 {
		t.Errorf("List should be sorted, got %v", ids)
	}
}

func TestUserStore_RemoveNonExistent(t *testing.T) {
	p := tempUserStorePath(t)
	u := NewUserStore(p)
	_ = u.Add(1)
	if err := u.Remove(999); err != nil {
		t.Errorf("removing non-existent ID should not error, got: %v", err)
	}
	if !u.Contains(1) {
		t.Error("existing ID should still be present after removing non-existent ID")
	}
}

func TestLoadConfig_AllowedUsernames(t *testing.T) {
	p := writeTemp(t, "TELEGRAM_BOT_TOKEN=t\nALLOWED_USER_IDS=1\nALLOWED_USERNAMES=alice, @bob, charlie\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedUsernames) != 3 {
		t.Errorf("AllowedUsernames = %v, want 3 items", cfg.AllowedUsernames)
	}
	for _, name := range cfg.AllowedUsernames {
		for _, c := range name {
			if c == '@' {
				t.Errorf("username should not contain @: %q", name)
			}
		}
	}
}

func TestIsAllowedByUsername(t *testing.T) {
	cfg := &Config{AllowedUsernames: []string{"alice", "bob"}}
	if !cfg.IsAllowedByUsername("alice") {
		t.Error("alice should be allowed")
	}
	if cfg.IsAllowedByUsername("charlie") {
		t.Error("charlie should not be allowed")
	}
	if cfg.IsAllowedByUsername("") {
		t.Error("empty username should not be allowed")
	}
}

func TestUserStore_FilePermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX permission bits not enforced on Windows")
	}
	p := tempUserStorePath(t)
	u := NewUserStore(p)
	_ = u.Add(1)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		t.Errorf("extra_users.json should not be world/group readable, got %o", perm)
	}
}
