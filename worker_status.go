package main

import (
	"fmt"
	"sync"
	"time"
)

// memoryWorkerStatusStore holds Worker status in memory (not persisted).
// Used for real-time monitoring of running and recent tasks.
type memoryWorkerStatusStore struct {
	mu       sync.RWMutex
	statuses map[string]WorkerStatus // key: "project:convID"
	recent   []WorkerStatus           // history of completed tasks (FIFO, max 50)
}

// NewMemoryWorkerStatusStore creates an in-memory Worker status tracker.
func NewMemoryWorkerStatusStore() WorkerStatusStore {
	return &memoryWorkerStatusStore{
		statuses: make(map[string]WorkerStatus),
		recent:   make([]WorkerStatus, 0, 50),
	}
}

// makeKey returns a unique key for (project, convID).
func makeKey(project, convID string) string {
	return project + ":" + convID
}

// GetStatus returns the current or last-known status of a Worker.
// It checks active statuses first, then recent history.
func (s *memoryWorkerStatusStore) GetStatus(project, convID string) (WorkerStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check active
	if status, ok := s.statuses[makeKey(project, convID)]; ok {
		return status, true
	}

	// Check recent history
	key := makeKey(project, convID)
	for _, status := range s.recent {
		if makeKey(status.Project, status.ConversationID) == key {
			return status, true
		}
	}

	return WorkerStatus{}, false
}

// SetStatus records a new Worker status (typically at start).
func (s *memoryWorkerStatusStore) SetStatus(status WorkerStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[makeKey(status.Project, status.ConversationID)] = status
	return nil
}

// UpdateStatus updates the status and optionally moves it to history.
func (s *memoryWorkerStatusStore) UpdateStatus(project, convID, newStatus, errorMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := makeKey(project, convID)
	status, ok := s.statuses[key]
	if !ok {
		return fmt.Errorf("worker status not found: %s", key)
	}

	status.Status = newStatus
	status.Error = errorMsg
	if newStatus != "running" {
		status.EndTime = time.Now()
	}

	// If completed/failed, archive to recent history
	if newStatus == "completed" || newStatus == "failed" || newStatus == "timeout" {
		delete(s.statuses, key)
		s.recent = append(s.recent, status)
		// Keep only last 50 completed tasks
		if len(s.recent) > 50 {
			s.recent = s.recent[len(s.recent)-50:]
		}
	} else {
		s.statuses[key] = status
	}

	return nil
}

// ListActive returns all currently running Workers.
func (s *memoryWorkerStatusStore) ListActive() []WorkerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	active := make([]WorkerStatus, 0, len(s.statuses))
	for _, status := range s.statuses {
		if status.Status == "running" {
			active = append(active, status)
		}
	}
	return active
}

// ListRecent returns the last N completed Workers.
func (s *memoryWorkerStatusStore) ListRecent(limit int) []WorkerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit > len(s.recent) {
		limit = len(s.recent)
	}
	// Return in reverse order (most recent first)
	result := make([]WorkerStatus, limit)
	for i := 0; i < limit; i++ {
		result[i] = s.recent[len(s.recent)-1-i]
	}
	return result
}
