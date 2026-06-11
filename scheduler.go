package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler manages Tasks (one-shot and recurring), persisting to tasks.json.
// Replaces the old Reminder + CronJob design.
type Scheduler struct {
	mu          sync.Mutex
	tasksPath   string
	tasks       []*Task
	cronRunner  *cron.Cron
	cronEntries map[string]cron.EntryID // Task.ID → cron EntryID
	stopChs     map[string]chan struct{} // Task.ID → cancel channel (one-shot)
	done        chan struct{}            // closed by Stop() to unblock Run()
	send        func(chatID int64, text string)
	dispatch    func(chatID int64, text string)
}

func NewScheduler(tasksPath string) *Scheduler {
	c := cron.New(cron.WithLocation(time.Local))
	return &Scheduler{
		tasksPath:   tasksPath,
		cronRunner:  c,
		cronEntries: make(map[string]cron.EntryID),
		done:        make(chan struct{}),
		stopChs:     make(map[string]chan struct{}),
	}
}

func (s *Scheduler) SetSend(f func(int64, string)) {
	s.mu.Lock()
	s.send = f
	s.mu.Unlock()
}

func (s *Scheduler) SetDispatch(f func(int64, string)) {
	s.mu.Lock()
	s.dispatch = f
	s.mu.Unlock()
}

// Load reads tasks.json; migrates schedule.json if tasks.json is absent.
func (s *Scheduler) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.tasksPath)
	if os.IsNotExist(err) {
		schedPath := filepath.Join(filepath.Dir(s.tasksPath), "schedule.json")
		if _, serr := os.Stat(schedPath); serr == nil {
			return s.migrateLegacy(schedPath)
		}
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.tasks)
}

// save writes tasks atomically. Lock must be held by caller.
func (s *Scheduler) save() error {
	b, err := json.MarshalIndent(s.tasks, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.tasksPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.tasksPath)
}

// Run registers all pending tasks and starts the cron runner. Call in a goroutine.
// Returns when Stop() is called.
func (s *Scheduler) Run() {
	s.mu.Lock()
	for _, t := range s.tasks {
		if t.Status == "pending" {
			if err := s.register(t); err != nil {
				log.Printf("[scheduler] startup register error for task %s: %v", t.ID, err)
			}
		}
	}
	s.mu.Unlock()
	s.cronRunner.Start()
	<-s.done
	s.cronRunner.Stop()
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	select {
	case <-s.done: // already closed
	default:
		close(s.done)
	}
}

// register adds a task to cronRunner (recurring) or starts a timer (one-shot).
// Returns an error if the cron expression is invalid.
// Lock must be held by caller.
func (s *Scheduler) register(t *Task) error {
	if t.CronExpr != "" {
		entryID, err := s.cronRunner.AddFunc(t.CronExpr, func() { s.fire(t.ID) })
		if err != nil {
			return fmt.Errorf("cron 식 오류 (%q): %w", t.CronExpr, err)
		}
		s.cronEntries[t.ID] = entryID
		return nil
	}
	if !t.FireAt.IsZero() {
		stopCh := make(chan struct{})
		s.stopChs[t.ID] = stopCh
		taskID := t.ID
		delay := time.Until(t.FireAt)
		if delay < 0 {
			delay = 0
		}
		go func() {
			select {
			case <-time.After(delay):
				s.fire(taskID)
				s.mu.Lock()
				s.removeByID(taskID)
				_ = s.save()
				s.mu.Unlock()
			case <-stopCh:
			}
		}()
	}
	return nil
}

// deregister removes a task from cronRunner or cancels its timer.
// Lock must be held by caller.
func (s *Scheduler) deregister(id string) {
	if entryID, ok := s.cronEntries[id]; ok {
		s.cronRunner.Remove(entryID)
		delete(s.cronEntries, id)
	}
	if ch, ok := s.stopChs[id]; ok {
		close(ch)
		delete(s.stopChs, id)
	}
}

// fire executes a task's action (called by cron tick or one-shot goroutine).
func (s *Scheduler) fire(taskID string) {
	s.mu.Lock()
	t := s.findByID(taskID)
	if t == nil || t.Status != "pending" {
		s.mu.Unlock()
		return
	}
	t.LastFired = time.Now()
	chatID, prompt, script, isTask := t.ChatID, t.Prompt, t.Script, t.IsTask
	_ = s.save()
	sendFn, dispatchFn := s.send, s.dispatch
	s.mu.Unlock()

	// Script pre-check: skip turn if wakeAgent == false
	if script != "" {
		wake, data := runScriptPrecheck(script)
		if !wake {
			log.Printf("[scheduler] task %s: wakeAgent=false — skipping this turn", taskID)
			return
		}
		if len(data) > 0 {
			b, _ := json.Marshal(data)
			prompt = prompt + "\n\n[Script data]: " + string(b)
		}
	}

	if isTask && dispatchFn != nil {
		go dispatchFn(chatID, prompt)
	} else if sendFn != nil {
		go sendFn(chatID, "🔔 "+prompt)
	}
}

// AddTask adds a new Task and registers it with the scheduler.
// Returns an error (and does not persist) if cron expression parsing fails.
func (s *Scheduler) AddTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Status == "pending" {
		if err := s.register(t); err != nil {
			return err
		}
	}
	s.tasks = append(s.tasks, t)
	return s.save()
}

// PauseTask pauses a task (deregisters, sets status="paused").
func (s *Scheduler) PauseTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findByID(id)
	if t == nil {
		return fmt.Errorf("작업 %q 없음", id)
	}
	s.deregister(id)
	t.Status = "paused"
	return s.save()
}

// ResumeTask re-activates a paused task.
// Idempotent: returns nil if the task is already pending (prevents ghost cron entries
// from leaking when register() would overwrite cronEntries without removing the old entry).
func (s *Scheduler) ResumeTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findByID(id)
	if t == nil {
		return fmt.Errorf("작업 %q 없음", id)
	}
	if t.Status == "pending" {
		return nil // already active — idempotent
	}
	t.Status = "pending"
	if err := s.register(t); err != nil {
		t.Status = "paused"
		return err
	}
	return s.save()
}

// CancelTask permanently cancels a task (deregisters, sets status="cancelled").
func (s *Scheduler) CancelTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findByID(id)
	if t == nil {
		return fmt.Errorf("작업 %q 없음", id)
	}
	s.deregister(id)
	t.Status = "cancelled"
	return s.save()
}

// UpdateTask updates mutable fields on an active or paused task.
// Pass empty string to leave a field unchanged.
func (s *Scheduler) UpdateTask(id, cronExpr, prompt, script string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findByID(id)
	if t == nil {
		return fmt.Errorf("작업 %q 없음", id)
	}
	wasActive := t.Status == "pending"
	// Snapshot old values so we can roll back if register() fails.
	oldCronExpr, oldPrompt, oldScript := t.CronExpr, t.Prompt, t.Script
	if wasActive {
		s.deregister(id)
	}
	if cronExpr != "" {
		t.CronExpr = cronExpr
	}
	if prompt != "" {
		t.Prompt = prompt
	}
	if script != "" {
		t.Script = script
	}
	if wasActive {
		if err := s.register(t); err != nil {
			// Roll back mutations so the task remains consistent.
			t.CronExpr, t.Prompt, t.Script = oldCronExpr, oldPrompt, oldScript
			t.Status = "paused" // can't re-register → mark paused to avoid phantom "pending"
			_ = s.save()
			return err
		}
	}
	return s.save()
}

// ListTasks returns tasks matching status filter ("pending"|"paused"|"cancelled"|"all"|"").
// "" and "all" return everything.
func (s *Scheduler) ListTasks(filter string) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if filter == "" || filter == "all" || t.Status == filter {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out
}

// NextFire returns the next scheduled fire time for a recurring task.
// Returns zero time if not found or not active.
func (s *Scheduler) NextFire(id string) time.Time {
	s.mu.Lock()
	entryID, ok := s.cronEntries[id]
	s.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	entry := s.cronRunner.Entry(entryID)
	return entry.Next
}

// Remove cancels a task by ID. Returns false if not found.
// Kept for backward compatibility with !remind / !cron commands.
func (s *Scheduler) Remove(id string) bool {
	err := s.CancelTask(id)
	return err == nil
}

// AddReminder creates a one-shot notification Task.
// Kept for backward compatibility with !remind.
func (s *Scheduler) AddReminder(chatID int64, msg string, fireAt time.Time) (*Task, error) {
	t := &Task{
		ID:        newTaskID(),
		ChatID:    chatID,
		Prompt:    msg,
		FireAt:    fireAt,
		Status:    "pending",
		IsTask:    false,
		Label:     "알림: " + msg,
		CreatedAt: time.Now(),
	}
	return t, s.AddTask(t)
}

// AddCron creates a recurring Task from a duration.
// Kept for backward compatibility with !cron.
func (s *Scheduler) AddCron(chatID int64, label string, interval time.Duration, task string, isTask bool) (*Task, error) {
	t := &Task{
		ID:        newTaskID(),
		ChatID:    chatID,
		Prompt:    task,
		CronExpr:  durationToCron(interval),
		Status:    "pending",
		IsTask:    isTask,
		Label:     label,
		CreatedAt: time.Now(),
	}
	return t, s.AddTask(t)
}

// ListReminders returns active one-shot tasks (CronExpr == "").
// Kept for backward compatibility with !remind list.
func (s *Scheduler) ListReminders() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if t.CronExpr == "" && t.Status == "pending" {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out
}

// ListCrons returns active recurring tasks (CronExpr != "").
// Kept for backward compatibility with !cron list.
func (s *Scheduler) ListCrons() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if t.CronExpr != "" && t.Status == "pending" {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out
}

// --- Legacy migration ---

type legacyScheduleData struct {
	Reminders []struct {
		ID      string    `json:"id"`
		ChatID  int64     `json:"chatId"`
		Message string    `json:"message"`
		FireAt  time.Time `json:"fireAt"`
	} `json:"reminders"`
	CronJobs []struct {
		ID       string        `json:"id"`
		ChatID   int64         `json:"chatId"`
		Label    string        `json:"label"`
		Interval time.Duration `json:"interval"`
		Task     string        `json:"task"`
		IsTask   bool          `json:"isTask"`
		Enabled  bool          `json:"enabled"`
	} `json:"cronJobs"`
}

// migrateLegacy converts schedule.json → tasks.json. Lock must be held by caller.
func (s *Scheduler) migrateLegacy(schedPath string) error {
	b, err := os.ReadFile(schedPath)
	if err != nil {
		return err
	}
	var old legacyScheduleData
	if err := json.Unmarshal(b, &old); err != nil {
		return fmt.Errorf("schedule.json 마이그레이션 파싱 실패: %w", err)
	}
	for _, r := range old.Reminders {
		s.tasks = append(s.tasks, &Task{
			ID:        "r-" + r.ID,
			ChatID:    r.ChatID,
			Prompt:    r.Message,
			FireAt:    r.FireAt,
			Status:    "pending",
			Label:     "알림: " + r.Message,
			CreatedAt: time.Now(),
		})
	}
	for _, c := range old.CronJobs {
		status := "pending"
		if !c.Enabled {
			status = "paused"
		}
		label := c.Label
		if label == "" {
			label = c.Task
		}
		s.tasks = append(s.tasks, &Task{
			ID:        "c-" + c.ID,
			ChatID:    c.ChatID,
			Prompt:    c.Task,
			CronExpr:  durationToCron(c.Interval),
			Status:    status,
			IsTask:    c.IsTask,
			Label:     label,
			CreatedAt: time.Now(),
		})
	}
	if err := s.save(); err != nil {
		return err
	}
	_ = os.Rename(schedPath, schedPath+".bak")
	log.Printf("[scheduler] migrated %d reminders + %d cron jobs from schedule.json", len(old.Reminders), len(old.CronJobs))
	return nil
}

// durationToCron converts a duration to a 5-field cron expression.
func durationToCron(d time.Duration) string {
	switch {
	case d <= time.Minute:
		return "* * * * *"
	case d < time.Hour:
		return fmt.Sprintf("*/%d * * * *", int(d.Minutes()))
	case d == time.Hour:
		return "0 * * * *"
	case d < 24*time.Hour:
		return fmt.Sprintf("0 */%d * * *", int(d.Hours()))
	case d == 24*time.Hour:
		return "0 0 * * *"
	case d == 7*24*time.Hour:
		return "0 0 * * 0"
	default:
		return fmt.Sprintf("@every %dm", int(d.Minutes()))
	}
}

// --- Script pre-check ---

type scriptResult struct {
	WakeAgent bool           `json:"wakeAgent"`
	Data      map[string]any `json:"data"`
}

// runScriptPrecheck runs a bash script and parses {"wakeAgent": bool, "data": {...}}.
// Returns wakeAgent=true on any failure (safe fallback: always run when uncertain).
func runScriptPrecheck(script string) (wakeAgent bool, data map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", script).Output()
	if err != nil {
		log.Printf("[scheduler] script precheck exec error: %v — defaulting wakeAgent=true", err)
		return true, nil
	}
	var res scriptResult
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		log.Printf("[scheduler] script precheck: invalid JSON %q — defaulting wakeAgent=true", strings.TrimSpace(string(out)))
		return true, nil
	}
	return res.WakeAgent, res.Data
}

// --- ParseSchedule (backward compat for !remind / !cron duration parsing) ---

// ParseSchedule parses duration strings: English ("30m", "2h", "1d", "1w", "hourly", "daily", "weekly")
// and Korean aliases ("매시간", "매일", "매주").
func ParseSchedule(raw string) (time.Duration, string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "hourly", "매시간":
		return time.Hour, "매시간", nil
	case "daily", "매일":
		return 24 * time.Hour, "매일", nil
	case "weekly", "매주":
		return 7 * 24 * time.Hour, "매주", nil
	}
	if len(raw) < 2 {
		return 0, "", fmt.Errorf("알 수 없는 형식: %q", raw)
	}
	unit := raw[len(raw)-1]
	var n int
	if _, err := fmt.Sscanf(raw[:len(raw)-1], "%d", &n); err != nil || n <= 0 {
		return 0, "", fmt.Errorf("잘못된 값: %q", raw)
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, fmt.Sprintf("%d분마다", n), nil
	case 'h':
		return time.Duration(n) * time.Hour, fmt.Sprintf("%d시간마다", n), nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, fmt.Sprintf("%d일마다", n), nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, fmt.Sprintf("%d주마다", n), nil
	}
	return 0, "", fmt.Errorf("알 수 없는 단위 '%c' — m/h/d/w/hourly/daily/weekly/매시간/매일/매주 사용", unit)
}

// --- Helpers ---

func (s *Scheduler) findByID(id string) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *Scheduler) removeByID(id string) {
	for i, t := range s.tasks {
		if t.ID == id {
			s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
			return
		}
	}
}

func newTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%08x", b)
}
