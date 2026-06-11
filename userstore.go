package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
)

// UserStore persists runtime-added allowed user IDs to extra_users.json.
// It complements Config.AllowedUserIDs (which is static / config-file-only).
type UserStore struct {
	mu      sync.Mutex
	path    string
	userIDs map[int64]bool
}

type userStoreData struct {
	UserIDs []int64 `json:"userIds"`
}

func NewUserStore(path string) *UserStore {
	return &UserStore{path: path, userIDs: make(map[int64]bool)}
}

func (u *UserStore) Load() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	data, err := os.ReadFile(u.path)
	if os.IsNotExist(err) {
		return nil // first run — no file yet
	}
	if err != nil {
		return fmt.Errorf("extra_users 로드 실패: %w", err)
	}
	var d userStoreData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("extra_users 파싱 실패: %w", err)
	}
	for _, id := range d.UserIDs {
		u.userIDs[id] = true
	}
	return nil
}

// snapshot returns a sorted copy of the current ID list. Lock must be held.
func (u *UserStore) snapshot() []int64 {
	ids := make([]int64, 0, len(u.userIDs))
	for id := range u.userIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// persist writes ids to disk without holding the mutex.
func (u *UserStore) persist(ids []int64) error {
	data, err := json.MarshalIndent(userStoreData{UserIDs: ids}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(u.path, data, 0o600)
}

func (u *UserStore) Add(id int64) error {
	u.mu.Lock()
	u.userIDs[id] = true
	ids := u.snapshot()
	u.mu.Unlock()
	return u.persist(ids)
}

func (u *UserStore) Remove(id int64) error {
	u.mu.Lock()
	delete(u.userIDs, id)
	ids := u.snapshot()
	u.mu.Unlock()
	return u.persist(ids)
}

func (u *UserStore) Contains(id int64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.userIDs[id]
}

func (u *UserStore) List() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	ids := make([]int64, 0, len(u.userIDs))
	for id := range u.userIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
