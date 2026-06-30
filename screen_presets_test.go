package main

import (
	"path/filepath"
	"testing"
)

func TestPresetSetGetList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presets.json")
	s := NewPresetStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file should be ok: %v", err)
	}

	if err := s.Set("settings", 100, 200); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("close", 10, 20); err != nil {
		t.Fatalf("Set: %v", err)
	}

	p, ok := s.Get("settings")
	if !ok {
		t.Fatalf("Get(settings) not found")
	}
	if p.Name != "settings" || p.X != 100 || p.Y != 200 {
		t.Fatalf("Get(settings) = %+v, want {settings 100 200}", p)
	}

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
}

func TestPresetGetMissing(t *testing.T) {
	dir := t.TempDir()
	s := NewPresetStore(filepath.Join(dir, "presets.json"))
	if _, ok := s.Get("nope"); ok {
		t.Fatalf("Get(missing) should be false")
	}
}

func TestPresetSaveReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presets.json")

	s := NewPresetStore(path)
	if err := s.Set("a", 1, 2); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("b", 3, 4); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fresh := NewPresetStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	a, ok := fresh.Get("a")
	if !ok || a.X != 1 || a.Y != 2 {
		t.Fatalf("reloaded a = %+v, ok=%v", a, ok)
	}
	b, ok := fresh.Get("b")
	if !ok || b.X != 3 || b.Y != 4 {
		t.Fatalf("reloaded b = %+v, ok=%v", b, ok)
	}
	if len(fresh.List()) != 2 {
		t.Fatalf("reloaded List() len = %d, want 2", len(fresh.List()))
	}
}
