package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Design Ref: §2 (preset_*), §1 (presets_file default ~/.teleclaude/presets.json).
//
// Preset is a named fixed-layout coordinate used by the preset_* screen tools.
type Preset struct {
	Name string `json:"name"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
}

// coord is the on-disk value for a preset (name is the map key).
type coord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// PresetStore is a mutex-protected, JSON-file-backed map of named coordinates.
type PresetStore struct {
	path string

	mu sync.RWMutex
	m  map[string]coord
}

// NewPresetStore returns a store backed by the given JSON file path. The file is
// not read until Load is called; the in-memory map starts empty.
func NewPresetStore(path string) *PresetStore {
	return &PresetStore{
		path: path,
		m:    make(map[string]coord),
	}
}

// Load reads the JSON file into memory. A missing file is treated as an empty
// store (not an error).
func (s *PresetStore) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.m = make(map[string]coord)
			s.mu.Unlock()
			return nil
		}
		return err
	}

	m := make(map[string]coord)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
	return nil
}

// Save writes the in-memory map to disk atomically (temp file + rename).
func (s *PresetStore) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.m, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	tmp, err := os.CreateTemp(dir, ".presets-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, s.path)
}

// Set stores (or overwrites) a preset and persists the store to disk.
func (s *PresetStore) Set(name string, x, y int) error {
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[string]coord)
	}
	s.m[name] = coord{X: x, Y: y}
	s.mu.Unlock()
	return s.Save()
}

// Get returns the preset for name and whether it exists.
func (s *PresetStore) Get(name string) (Preset, bool) {
	s.mu.RLock()
	c, ok := s.m[name]
	s.mu.RUnlock()
	if !ok {
		return Preset{}, false
	}
	return Preset{Name: name, X: c.X, Y: c.Y}, true
}

// List returns all presets sorted by name.
func (s *PresetStore) List() []Preset {
	s.mu.RLock()
	out := make([]Preset, 0, len(s.m))
	for name, c := range s.m {
		out = append(out, Preset{Name: name, X: c.X, Y: c.Y})
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// defaultPresetsPath returns ~/.teleclaude/presets.json (reusing dataDir()).
func defaultPresetsPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "presets.json"), nil
}
