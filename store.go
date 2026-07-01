package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Design Ref: §4.1 StoreRepo, §3.4 store.json. Infrastructure layer.

// fileStore is a JSON-file backed StoreRepo (MVP). Safe for concurrent use.
type fileStore struct {
	path string
	mu   sync.Mutex
	data StoreData
}

// NewFileStore creates a store backed by the given JSON file path.
func NewFileStore(path string) *fileStore {
	return &fileStore{
		path: path,
		data: StoreData{Projects: map[string]*Project{}},
	}
}

// Load reads store.json. A missing file is treated as an empty store.
func (s *fileStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.data = StoreData{Projects: map[string]*Project{}}
			return nil
		}
		return err
	}
	var d StoreData
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("store.json 파싱 실패: %w", err)
	}
	if d.Projects == nil {
		d.Projects = map[string]*Project{}
	}
	for _, p := range d.Projects {
		if p.Conversations == nil {
			p.Conversations = map[string]*Conversation{}
		}
	}
	s.data = d
	return nil
}

// Save writes store.json atomically (temp file + rename).
func (s *fileStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *fileStore) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ListProjects returns a shallow copy of the project map.
func (s *fileStore) ListProjects() map[string]*Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*Project, len(s.data.Projects))
	maps.Copy(out, s.data.Projects)
	return out
}

// AddProject registers a directory under a name. The path must exist and be a directory.
func (s *fileStore) AddProject(name, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == "" {
		return fmt.Errorf("프로젝트 이름이 비어 있습니다")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("경로 변환 실패: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("디렉토리가 존재하지 않습니다: %s", abs)
	}
	if _, exists := s.data.Projects[name]; exists {
		return fmt.Errorf("이미 등록된 프로젝트입니다: %s", name)
	}
	s.data.Projects[name] = &Project{Path: abs, Conversations: map[string]*Conversation{}}
	return s.saveLocked()
}

// RemoveProject deletes a project (and its conversation metadata).
func (s *fileStore) RemoveProject(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Projects[name]; !ok {
		return fmt.Errorf("프로젝트를 찾을 수 없습니다: %s", name)
	}
	delete(s.data.Projects, name)
	if s.data.Active.Project == name {
		s.data.Active = ActiveRef{}
	}
	return s.saveLocked()
}

// GetProject returns a project by name.
func (s *fileStore) GetProject(name string) (*Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[name]
	return p, ok
}

// NewConversation creates a conversation in a project, assigning a numeric ID and a session UUID.
func (s *fileStore) NewConversation(project, title string) (*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.data.Projects[project]
	if !ok {
		return nil, fmt.Errorf("프로젝트를 찾을 수 없습니다: %s", project)
	}
	id := nextConvID(p.Conversations)
	if title == "" {
		title = "대화 " + id
	}
	c := &Conversation{
		ID:           id,
		Title:        title,
		SessionID:    newUUID(),
		Started:      false,
		LastActivity: time.Now().UTC(),
	}
	p.Conversations[id] = c
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// nextConvID returns the smallest unused positive integer ID as a string.
func nextConvID(convs map[string]*Conversation) string {
	max := 0
	for k := range convs {
		if n, err := strconv.Atoi(k); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1)
}

// GetConversation returns a conversation within a project.
func (s *fileStore) GetConversation(project, convID string) (*Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if !ok {
		return nil, false
	}
	c, ok := p.Conversations[convID]
	return c, ok
}

// UpdateConversation persists changes to a conversation.
func (s *fileStore) UpdateConversation(project string, c *Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if !ok {
		return fmt.Errorf("프로젝트를 찾을 수 없습니다: %s", project)
	}
	p.Conversations[c.ID] = c
	return s.saveLocked()
}

// SetActive records the active project/conversation pointer.
func (s *fileStore) SetActive(project, convID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Active = ActiveRef{Project: project, ConversationID: convID}
	return s.saveLocked()
}

// GetActive returns the active pointer.
func (s *fileStore) GetActive() ActiveRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Active
}

// sortedConvIDsByActivity returns IDs sorted by LastActivity descending (most recent first).
func sortedConvIDsByActivity(convs map[string]*Conversation) []string {
	ids := make([]string, 0, len(convs))
	for k := range convs {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		return convs[ids[i]].LastActivity.After(convs[ids[j]].LastActivity)
	})
	return ids
}

// sortedConvIDs returns conversation IDs in numeric order (helper for listings).
func sortedConvIDs(convs map[string]*Conversation) []string {
	ids := make([]string, 0, len(convs))
	for k := range convs {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		ni, _ := strconv.Atoi(ids[i])
		nj, _ := strconv.Atoi(ids[j])
		return ni < nj
	})
	return ids
}

// GetStoredBackend returns the persisted backend name ("claude"|"codex"; "" = claude).
func (s *fileStore) GetStoredBackend() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ActiveBackend
}

// SetStoredBackend persists the active backend to store.json.
func (s *fileStore) SetStoredBackend(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ActiveBackend = name
	return s.saveLocked()
}

// PruneOldConversations removes conversations whose LastActivity is older than
// ttlDays, skipping the currently active conversation. ttlDays <= 0 disables
// pruning (returns 0, nil). Returns the number of conversations removed.
func (s *fileStore) PruneOldConversations(ttlDays int) (int, error) {
	if ttlDays <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().UTC().AddDate(0, 0, -ttlDays)
	removed := 0
	for projName, p := range s.data.Projects {
		for id, c := range p.Conversations {
			if projName == s.data.Active.Project && id == s.data.Active.ConversationID {
				continue
			}
			if c.LastActivity.Before(cutoff) {
				delete(p.Conversations, id)
				removed++
			}
		}
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, s.saveLocked()
}

// GetParent returns the parent conversation in a chain (used for continuation context).
func (s *fileStore) GetParent(project, convID string) (*Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Projects[project]
	if !ok {
		return nil, false
	}
	c, ok := p.Conversations[convID]
	if !ok || c.ParentID == "" {
		return nil, false
	}
	parent, ok := p.Conversations[c.ParentID]
	return parent, ok
}
