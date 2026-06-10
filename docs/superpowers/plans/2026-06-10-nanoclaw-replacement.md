# Nanoclaw Replacement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** nanoclaw를 Go 단일 바이너리로 대체 — 표준 cron expression, script precheck, Task pause/resume/update, 첨부파일 처리, 대화 히스토리, Linux 크로스플랫폼

**Architecture:** `Reminder`+`CronJob` → 단일 `Task` 타입으로 통합. `robfig/cron/v3`으로 cron 실행. 플랫폼별 프로세스 관리를 `platform_windows.go` / `platform_linux.go` build tag 파일로 분리.

**Tech Stack:** Go 1.25, github.com/robfig/cron/v3, github.com/go-telegram-bot-api/telegram-bot-api/v5

---

## 파일 맵

| 파일 | 역할 |
|------|------|
| `types.go` | `Task` 타입 추가 |
| `platform_windows.go` | Windows 프로세스 관리 (build tag) |
| `platform_linux.go` | Linux 프로세스 관리 (build tag) |
| `config.go` | `findClaude` Linux 경로 추가, `findClaudeOS` 위임 |
| `scheduler.go` | 전면 재작성 — Task CRUD, cron/one-shot 실행, script precheck |
| `history.go` | 신규 — 날짜별 대화 히스토리 저장/조회 |
| `bot.go` | `!task` 명령, 첨부파일 핸들러, `!history` 명령 추가 |
| `manager.go` | Worker 완료 후 히스토리 저장 호출 |
| `main.go` | platform 함수 호출로 교체, `killPreviousInstance`/`waitForProcessExit` 삭제 |
| `runner.go` | `killTree` 삭제 → platform 함수 호출 |
| `go.mod` | `robfig/cron/v3` 추가 |

---

## Task 1: 의존성 추가 + Task 타입

**Files:**
- Modify: `go.mod`
- Modify: `types.go`

- [ ] **Step 1: robfig/cron/v3 추가**

```powershell
cd "c:\Project\88.MyProject\Teleclaude"
go get github.com/robfig/cron/v3@v3.0.1
```

Expected: `go.mod`에 `require github.com/robfig/cron/v3 v3.0.1` 추가됨

- [ ] **Step 2: Task 타입을 types.go에 추가**

`types.go`의 `// --- Worker run I/O` 주석 앞에 삽입:

```go
// Task is a unified scheduled item replacing Reminder and CronJob.
// CronExpr != "" → recurring (robfig/cron/v3 syntax, e.g. "0 9 * * 1-5").
// CronExpr == "" → one-shot (FireAt used).
// Status: "pending" | "paused" | "cancelled"
type Task struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chatId"`
	Prompt    string    `json:"prompt"`
	Script    string    `json:"script,omitempty"`   // bash pre-check; empty = skip
	CronExpr  string    `json:"cronExpr,omitempty"` // standard 5-field cron
	FireAt    time.Time `json:"fireAt,omitempty"`   // one-shot: when to fire
	Status    string    `json:"status"`
	IsTask    bool      `json:"isTask"` // true = Claude Worker, false = notify
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	LastFired time.Time `json:"lastFired,omitempty"`
}
```

- [ ] **Step 3: 빌드 확인**

```powershell
go build ./...
```

Expected: 에러 없음

- [ ] **Step 4: 커밋**

```powershell
git add go.mod go.sum types.go
git commit -m "feat: add Task type + robfig/cron/v3 dependency"
```

---

## Task 2: Platform 추상화 파일 생성

**Files:**
- Create: `platform_windows.go`
- Create: `platform_linux.go`

- [ ] **Step 1: platform_windows.go 생성**

```go
//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const exeSuffix = ".exe"

func killTree(pid int) error {
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

func killPreviousInstance() {
	myPID := os.Getpid()
	killed := false

	if b, err := os.ReadFile(pidFilePath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != myPID {
			if exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run() == nil {
				log.Printf("[main] killed previous instance via PID file (PID %d)", pid)
				killed = true
			}
		}
	}

	for _, name := range []string{"teleclaude.exe", "teleclaude_new.exe"} {
		out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name, "/FO", "CSV", "/NH").CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(strings.ToLower(line), "info:") {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}
			pid, err := strconv.Atoi(strings.Trim(parts[1], `"`))
			if err != nil || pid <= 0 || pid == myPID {
				continue
			}
			if exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run() == nil {
				log.Printf("[main] killed competing %s (PID %d)", name, pid)
				killed = true
			}
		}
	}

	if killed {
		time.Sleep(3 * time.Second)
	}
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		out, _ := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").CombinedOutput()
		alive := false
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(strings.ToLower(line), "info:") {
				alive = true
				break
			}
		}
		if !alive {
			log.Printf("[main] old process (PID %d) has exited", pid)
			return
		}
	}
	exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	log.Printf("[main] force-killed old process (PID %d) after timeout", pid)
}

// findClaudeOS returns Windows-specific candidate paths for the claude CLI.
func findClaudeOS() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, "AppData", "Roaming", "npm", "claude.cmd"),
		filepath.Join(home, "AppData", "Roaming", "npm", "claude.exe"),
		filepath.Join(home, ".local", "bin", "claude.exe"),
		`C:\Program Files\nodejs\claude.cmd`,
	}
}
```

- [ ] **Step 2: platform_linux.go 생성**

```go
//go:build linux || darwin

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const exeSuffix = ""

func killTree(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

func killPreviousInstance() {
	myPID := os.Getpid()
	killed := false

	if b, err := os.ReadFile(pidFilePath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != myPID {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				log.Printf("[main] sent SIGTERM to previous instance (PID %d)", pid)
				killed = true
			}
		}
	}

	out, _ := exec.Command("pgrep", "-x", "teleclaude").CombinedOutput()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 || pid == myPID {
			continue
		}
		if syscall.Kill(pid, syscall.SIGTERM) == nil {
			log.Printf("[main] killed competing teleclaude (PID %d)", pid)
			killed = true
		}
	}

	if killed {
		time.Sleep(2 * time.Second)
	}
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/status", pid)); os.IsNotExist(err) {
			log.Printf("[main] old process (PID %d) has exited", pid)
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	log.Printf("[main] force-killed old process (PID %d) after timeout", pid)
}

// findClaudeOS returns Linux/macOS-specific candidate paths for the claude CLI.
func findClaudeOS() []string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		"/usr/local/bin/claude",
		filepath.Join(home, ".npm-global", "bin", "claude"),
		"/usr/bin/claude",
	}
	// nvm 경로 탐색
	nvmBase := filepath.Join(home, ".nvm", "versions", "node")
	if entries, err := os.ReadDir(nvmBase); err == nil {
		for _, e := range entries {
			candidates = append(candidates,
				filepath.Join(nvmBase, e.Name(), "bin", "claude"))
		}
	}
	return candidates
}
```

- [ ] **Step 3: 빌드 확인 (Windows)**

```powershell
go build ./...
```

Expected: 에러 없음

- [ ] **Step 4: Cross-compile Linux 확인**

```powershell
$env:GOOS="linux"; $env:GOARCH="arm64"; go build -o teleclaude_linux_arm64 .; Remove-Item teleclaude_linux_arm64
$env:GOOS=""; $env:GOARCH=""
```

Expected: 에러 없음 (바이너리 생성 후 삭제)

- [ ] **Step 5: 커밋**

```powershell
git add platform_windows.go platform_linux.go
git commit -m "feat: platform abstraction — Windows/Linux process management split"
```

---

## Task 3: main.go + runner.go 정리

**Files:**
- Modify: `main.go`
- Modify: `runner.go`
- Modify: `config.go`

- [ ] **Step 1: main.go에서 killPreviousInstance, waitForProcessExit 삭제**

`main.go`에서 다음 함수들을 **완전히 삭제**:
- `killPreviousInstance()` 함수 전체 (86~126번 줄)
- `waitForProcessExit()` 함수 전체 (130~151번 줄)

platform_windows.go / platform_linux.go에 이미 정의되어 있으므로 중복 제거.

- [ ] **Step 2: main.go selfRename에서 .exe 하드코딩 제거**

```go
// 기존
if filepath.Base(currentExe) == "teleclaude_new.exe" {
    go selfRename(currentExe, bot, notifyChatID)
}

// 변경 후
if filepath.Base(currentExe) == "teleclaude_new"+exeSuffix {
    go selfRename(currentExe, bot, notifyChatID)
}
```

- [ ] **Step 3: selfRename 함수에서 .exe 하드코딩 제거**

```go
// 기존
func selfRename(currentExe string, bot *Bot, notifyChatID int64) {
    target := filepath.Join(filepath.Dir(currentExe), "teleclaude.exe")
    ...
    if filepath.Base(exe) == "teleclaude_new.exe" {
    ...

// 변경 후
func selfRename(currentExe string, bot *Bot, notifyChatID int64) {
    target := filepath.Join(filepath.Dir(currentExe), "teleclaude"+exeSuffix)
    ...
    if filepath.Base(exe) == "teleclaude_new"+exeSuffix {
    ...
```

- [ ] **Step 4: runner.go에서 killTree 삭제**

`runner.go`에서 다음 함수 **완전히 삭제**:
```go
// killTree force-kills a process and its children on Windows.
func killTree(pid int) error {
    return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
```

platform 파일에 이미 정의되어 있으므로 중복 제거.

- [ ] **Step 5: runner.go import 정리**

`runner.go`에서 더 이상 필요 없는 import 제거:
- `strconv`가 killTree 삭제 후 다른 곳에서 쓰이는지 확인. 쓰이지 않으면 삭제.

실제로 `runner.go`는 `strconv`를 killTree에서만 사용하므로 삭제.

- [ ] **Step 6: config.go findClaude 수정**

기존 Windows 전용 candidates를 `findClaudeOS()` 호출로 교체:

```go
func findClaude(explicit string) (string, error) {
    if explicit != "" {
        if _, err := os.Stat(explicit); err == nil {
            return explicit, nil
        }
        return "", fmt.Errorf("CLAUDE_PATH가 존재하지 않습니다: %s", explicit)
    }
    if p, err := exec.LookPath("claude"); err == nil {
        return p, nil
    }
    for _, c := range findClaudeOS() {
        if _, err := os.Stat(c); err == nil {
            return c, nil
        }
    }
    return "", fmt.Errorf("claude CLI를 찾을 수 없습니다. PATH에 추가하거나 CLAUDE_PATH를 설정하세요")
}
```

- [ ] **Step 7: bot.go handleUpdate에서 .exe 하드코딩 제거**

```go
// 기존
newExe := filepath.Join(srcDir, "teleclaude_new.exe")
...
if filepath.Base(exe) == "teleclaude_new.exe" {

// 변경 후
newExe := filepath.Join(srcDir, "teleclaude_new"+exeSuffix)
...
if filepath.Base(exe) == "teleclaude_new"+exeSuffix {
```

- [ ] **Step 8: 빌드 확인**

```powershell
go build ./...
$env:GOOS="linux"; $env:GOARCH="arm64"; go build -o teleclaude_linux_arm64 .; Remove-Item teleclaude_linux_arm64
$env:GOOS=""; $env:GOARCH=""
```

Expected: 두 플랫폼 모두 에러 없음

- [ ] **Step 9: 커밋**

```powershell
git add main.go runner.go config.go bot.go
git commit -m "refactor: remove platform-specific code from main/runner/config, use platform abstraction"
```

---

## Task 4: Scheduler 전면 재작성

**Files:**
- Rewrite: `scheduler.go`

- [ ] **Step 1: scheduler.go 전체를 다음 내용으로 교체**

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduleStore is the JSON layout for tasks.json.
type scheduleStore struct {
	Tasks []*Task `json:"tasks"`
}

// legacyScheduleData is used only for migrating old schedule.json.
type legacyScheduleData struct {
	Reminders []*Reminder `json:"reminders"`
	CronJobs  []*CronJob  `json:"cronJobs"`
}

// Scheduler manages Tasks, persisting to tasks.json.
type Scheduler struct {
	mu          sync.Mutex
	path        string // tasks.json path
	tasks       []*Task
	cronRunner  *cron.Cron
	cronEntries map[string]cron.EntryID // Task.ID → cron EntryID
	stopCh      chan struct{}
	send        func(chatID int64, text string)
	dispatch    func(chatID int64, text string)
}

func NewScheduler(dir string) *Scheduler {
	return &Scheduler{
		path:        dir + "/tasks.json",
		cronEntries: make(map[string]cron.EntryID),
		stopCh:      make(chan struct{}),
		cronRunner:  cron.New(),
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

// Load reads tasks.json; also migrates legacy schedule.json if present.
func (s *Scheduler) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	legacyPath := strings.TrimSuffix(s.path, "tasks.json") + "schedule.json"
	_ = s.migrateLegacy(legacyPath)

	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var store scheduleStore
	if err := json.Unmarshal(b, &store); err != nil {
		return err
	}
	s.tasks = store.Tasks
	return nil
}

// migrateLegacy converts old schedule.json (Reminder + CronJob) to Task format.
// Must be called with s.mu held.
func (s *Scheduler) migrateLegacy(legacyPath string) error {
	b, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy legacyScheduleData
	if err := json.Unmarshal(b, &legacy); err != nil {
		return err
	}

	for _, r := range legacy.Reminders {
		s.tasks = append(s.tasks, &Task{
			ID:        "migrated-" + r.ID,
			ChatID:    r.ChatID,
			Prompt:    r.Message,
			FireAt:    r.FireAt,
			Status:    "pending",
			IsTask:    false,
			Label:     "알림 (마이그레이션)",
			CreatedAt: time.Now(),
		})
	}

	for _, c := range legacy.CronJobs {
		cronExpr := durationToCron(c.Interval)
		s.tasks = append(s.tasks, &Task{
			ID:        "migrated-" + c.ID,
			ChatID:    c.ChatID,
			Prompt:    c.Task,
			CronExpr:  cronExpr,
			Status:    taskStatus(c.Enabled),
			IsTask:    c.IsTask,
			Label:     c.Label,
			CreatedAt: time.Now(),
		})
	}

	if len(legacy.Reminders)+len(legacy.CronJobs) > 0 {
		if err := s.save(); err != nil {
			log.Printf("[scheduler] migration save error: %v", err)
		}
		_ = os.Rename(legacyPath, legacyPath+".bak")
		log.Printf("[scheduler] migrated %d reminders + %d cron jobs from schedule.json",
			len(legacy.Reminders), len(legacy.CronJobs))
	}
	return nil
}

func taskStatus(enabled bool) string {
	if enabled {
		return "pending"
	}
	return "paused"
}

// durationToCron converts a time.Duration to a cron expression (best-effort).
func durationToCron(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "* * * * *"
	case d%time.Hour == 0 && d < 24*time.Hour:
		h := int(d / time.Hour)
		if h == 1 {
			return "0 * * * *"
		}
		return fmt.Sprintf("0 */%d * * *", h)
	case d == 24*time.Hour:
		return "0 0 * * *"
	case d == 7*24*time.Hour:
		return "0 0 * * 0"
	default:
		mins := int(d.Minutes())
		if mins < 60 {
			return fmt.Sprintf("*/%d * * * *", mins)
		}
		return fmt.Sprintf("@every %dm", mins)
	}
}

func (s *Scheduler) save() error {
	store := scheduleStore{Tasks: s.tasks}
	b, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Scheduler) newTaskID() string {
	return fmt.Sprintf("task-%d", time.Now().UnixMilli())
}

// Run starts the cron runner and schedules all loaded tasks. Call in a goroutine.
func (s *Scheduler) Run() {
	s.mu.Lock()
	for _, t := range s.tasks {
		if t.Status == "pending" {
			s.register(t)
		}
	}
	s.mu.Unlock()
	s.cronRunner.Start()
	<-s.stopCh
	s.cronRunner.Stop()
}

// register adds a task to the cron runner. Must be called with s.mu held.
func (s *Scheduler) register(t *Task) {
	if t.CronExpr != "" {
		entryID, err := s.cronRunner.AddFunc(t.CronExpr, func() { s.fireTask(t) })
		if err != nil {
			log.Printf("[scheduler] cron parse error for task %s: %v", t.ID, err)
			return
		}
		s.cronEntries[t.ID] = entryID
	} else {
		go s.runOneShotTask(t)
	}
}

// unregister removes a task from the cron runner. Must be called with s.mu held.
func (s *Scheduler) unregister(id string) {
	if entryID, ok := s.cronEntries[id]; ok {
		s.cronRunner.Remove(entryID)
		delete(s.cronEntries, id)
	}
}

// runOneShotTask waits until t.FireAt and fires once.
func (s *Scheduler) runOneShotTask(t *Task) {
	dur := time.Until(t.FireAt)
	if dur <= 0 {
		return
	}
	select {
	case <-time.After(dur):
		s.mu.Lock()
		if t.Status != "pending" {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.fireTask(t)
		s.mu.Lock()
		t.Status = "cancelled"
		t.LastFired = time.Now()
		_ = s.save()
		s.mu.Unlock()
	case <-s.stopCh:
		return
	}
}

// fireTask executes a task: runs script precheck if set, then send/dispatch.
func (s *Scheduler) fireTask(t *Task) {
	s.mu.Lock()
	if t.Status != "pending" {
		s.mu.Unlock()
		return
	}
	chatID, prompt, script, isTask := t.ChatID, t.Prompt, t.Script, t.IsTask
	t.LastFired = time.Now()
	_ = s.save()
	s.mu.Unlock()

	// Script precheck
	if script != "" {
		wake, data, err := runScript(context.Background(), script)
		if err != nil {
			log.Printf("[scheduler] task %s script error: %v", t.ID, err)
		}
		if !wake {
			log.Printf("[scheduler] task %s skipped (wakeAgent=false)", t.ID)
			return
		}
		if data != "" && data != "null" {
			prompt = prompt + "\n\n스크립트 데이터:\n" + data
		}
	}

	if isTask {
		s.mu.Lock()
		dispatch := s.dispatch
		s.mu.Unlock()
		if dispatch != nil {
			dispatch(chatID, prompt)
		}
	} else {
		s.mu.Lock()
		send := s.send
		s.mu.Unlock()
		if send != nil {
			send(chatID, "🔔 "+prompt)
		}
	}
}

// runScript executes a bash pre-check script and returns the wakeAgent decision.
// Returns wakeAgent=true on any error (safe default: always wake if check fails).
func runScript(ctx context.Context, script string) (wakeAgent bool, data string, err error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var cmd interface{ Output() ([]byte, error) }
	if runtime.GOOS == "windows" {
		cmd = execCmd(ctx2, "cmd", "/C", script)
	} else {
		cmd = execCmd(ctx2, "bash", "-c", script)
	}

	out, runErr := cmd.Output()
	if runErr != nil {
		return true, "", runErr
	}

	var result struct {
		WakeAgent bool            `json:"wakeAgent"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return true, "", nil
	}
	return result.WakeAgent, string(result.Data), nil
}

// execCmd is a thin wrapper so runScript stays testable.
var execCmd = func(ctx context.Context, name string, args ...string) interface{ Output() ([]byte, error) } {
	return execCmdImpl{ctx: ctx, name: name, args: args}
}

type execCmdImpl struct {
	ctx  context.Context
	name string
	args []string
}

func (e execCmdImpl) Output() ([]byte, error) {
	import_exec := func() ([]byte, error) {
		var cmd = newOsExecCmd(e.ctx, e.name, e.args...)
		return cmd.Output()
	}
	return import_exec()
}
```

**Note:** 위 execCmdImpl은 컴파일 에러가 남. 아래 Step 2에서 수정.

- [ ] **Step 2: execCmd 부분을 단순화하여 scheduler.go 완성**

`runScript` 함수와 execCmd를 다음과 같이 교체:

```go
// runScript executes a bash pre-check script and returns the wakeAgent decision.
func runScript(ctx context.Context, script string) (wakeAgent bool, data string, err error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var args []string
	var name string
	if runtime.GOOS == "windows" {
		name = "cmd"
		args = []string{"/C", script}
	} else {
		name = "bash"
		args = []string{"-c", script}
	}

	out, runErr := newExecCommand(ctx2, name, args...).Output()
	if runErr != nil {
		return true, "", runErr
	}

	var result struct {
		WakeAgent bool            `json:"wakeAgent"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return true, "", nil
	}
	return result.WakeAgent, string(result.Data), nil
}
```

그리고 파일 상단 imports에 `"os/exec"` 추가 후, `newExecCommand` 변수 추가:

```go
// newExecCommand is replaceable for testing.
var newExecCommand = func(ctx context.Context, name string, args ...string) interface{ Output() ([]byte, error) } {
	return exec.CommandContext(ctx, name, args...)
}
```

- [ ] **Step 3: CRUD 메서드 추가 (scheduler.go 끝에 추가)**

```go
// AddTask registers a new task and persists it.
func (s *Scheduler) AddTask(t *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = s.newTaskID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.Status == "" {
		t.Status = "pending"
	}
	s.tasks = append(s.tasks, t)
	if t.Status == "pending" {
		s.register(t)
	}
	return s.save()
}

// PauseTask suspends a task without removing it.
func (s *Scheduler) PauseTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findTask(id)
	if t == nil {
		return fmt.Errorf("task not found: %s", id)
	}
	s.unregister(id)
	t.Status = "paused"
	return s.save()
}

// ResumeTask re-activates a paused task.
func (s *Scheduler) ResumeTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findTask(id)
	if t == nil {
		return fmt.Errorf("task not found: %s", id)
	}
	t.Status = "pending"
	s.register(t)
	return s.save()
}

// CancelTask permanently removes a task.
func (s *Scheduler) CancelTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unregister(id)
	for i, t := range s.tasks {
		if t.ID == id {
			s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("task not found: %s", id)
}

// UpdateTask modifies fields of an existing task.
type TaskUpdate struct {
	Prompt   *string
	CronExpr *string
	Script   *string
	FireAt   *time.Time
}

func (s *Scheduler) UpdateTask(id string, u TaskUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findTask(id)
	if t == nil {
		return fmt.Errorf("task not found: %s", id)
	}
	wasActive := t.Status == "pending"
	if wasActive {
		s.unregister(id)
	}
	if u.Prompt != nil {
		t.Prompt = *u.Prompt
	}
	if u.CronExpr != nil {
		t.CronExpr = *u.CronExpr
	}
	if u.Script != nil {
		t.Script = *u.Script
	}
	if u.FireAt != nil {
		t.FireAt = *u.FireAt
	}
	if wasActive {
		s.register(t)
	}
	return s.save()
}

// ListTasks returns tasks filtered by status ("pending", "paused", or "" for all).
func (s *Scheduler) ListTasks(statusFilter string) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if statusFilter == "" || t.Status == statusFilter {
			out = append(out, t)
		}
	}
	return out
}

// findTask returns the task pointer by ID. Must be called with s.mu held.
func (s *Scheduler) findTask(id string) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// --- Compatibility wrappers for legacy !remind / !cron commands ---

// AddReminder creates a one-shot notification Task (replaces old Reminder).
func (s *Scheduler) AddReminder(chatID int64, msg string, fireAt time.Time) (*Task, error) {
	t := &Task{
		ChatID: chatID,
		Prompt: msg,
		FireAt: fireAt,
		IsTask: false,
		Label:  "알림",
	}
	return t, s.AddTask(t)
}

// AddCron creates a recurring Task (replaces old CronJob).
func (s *Scheduler) AddCron(chatID int64, label string, interval time.Duration, task string, isTask bool) (*Task, error) {
	t := &Task{
		ChatID:   chatID,
		Prompt:   task,
		CronExpr: durationToCron(interval),
		IsTask:   isTask,
		Label:    label,
	}
	return t, s.AddTask(t)
}

// Remove cancels a task by ID (legacy compatibility for !remind cancel / !cron remove).
func (s *Scheduler) Remove(id string) bool {
	return s.CancelTask(id) == nil
}

// ListReminders returns one-shot pending tasks (legacy compatibility).
func (s *Scheduler) ListReminders() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if t.CronExpr == "" && t.Status == "pending" {
			out = append(out, t)
		}
	}
	return out
}

// ListCrons returns recurring tasks (legacy compatibility).
func (s *Scheduler) ListCrons() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Task
	for _, t := range s.tasks {
		if t.CronExpr != "" && t.Status != "cancelled" {
			out = append(out, t)
		}
	}
	return out
}
```

- [ ] **Step 4: bot.go의 !remind, !cron 호환 수정**

`bot.go`의 `handleRemind`에서 `_ = isTask` 줄을 실제 dispatch로 교체:

```go
// 기존 (bot.go ~462)
_ = isTask // isTask reminders use same path for now — sends notification
_ = b.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후: %s", r.ID, dur.Round(time.Second), msg))

// 변경 후
if isTask {
    // task 타입: dispatch를 통해 Claude가 실행
    _ = b.Send(chatID, fmt.Sprintf("✅ 작업 예약 [%s] — %s 후: %s", r.ID, dur.Round(time.Second), msg))
} else {
    _ = b.Send(chatID, fmt.Sprintf("✅ 알림 등록 [%s] — %s 후: %s", r.ID, dur.Round(time.Second), msg))
}
```

또한 `AddReminder` 호출 시 isTask를 반영:

```go
// 기존
r, err := b.scheduler.AddReminder(chatID, msg, fireAt)

// 변경 후 (Task 반환 타입으로)
t := &Task{
    ChatID: chatID,
    Prompt: msg,
    FireAt: fireAt,
    IsTask: isTask,
    Label:  "알림",
}
err := b.scheduler.AddTask(t)
if err != nil {
    _ = b.Send(chatID, "⚠️ 알림 등록 실패: "+err.Error())
    return
}
r := t
```

- [ ] **Step 5: bot.go의 !remind list 호환 수정**

```go
// 기존 (ListReminders → *Reminder)
for _, r := range reminders {
    remaining := time.Until(r.FireAt).Round(time.Second)
    fmt.Fprintf(&sb, "[%s] %s 후 — %s\n", r.ID, remaining, r.Message)
}

// 변경 후 (ListReminders → *Task)
for _, r := range reminders {
    remaining := time.Until(r.FireAt).Round(time.Second)
    fmt.Fprintf(&sb, "[%s] %s 후 — %s\n", r.ID, remaining, r.Prompt)
}
```

- [ ] **Step 6: bot.go의 !cron list 호환 수정**

```go
// 기존 (ListCrons → *CronJob)
for _, c := range crons {
    kind := "알림"
    if c.IsTask { kind = "작업" }
    next := time.Until(c.NextFire).Round(time.Second)
    fmt.Fprintf(&sb, "[%s] %s (%s) — 다음: %s 후\n  %s\n", c.ID, c.Label, kind, next, c.Task)
}

// 변경 후 (ListCrons → *Task)
for _, c := range crons {
    kind := "알림"
    if c.IsTask { kind = "작업" }
    // NextFire: cron entry에서 가져오거나 LastFired + interval 계산
    // 간단하게 cron expression 표시
    fmt.Fprintf(&sb, "[%s] %s (%s) — %s\n  %s\n", c.ID, c.Label, kind, c.CronExpr, c.Prompt)
}
```

- [ ] **Step 7: main.go scheduler 초기화 수정**

```go
// 기존
sched := NewScheduler(filepath.Join(dir, "schedule.json"))

// 변경 후
sched := NewScheduler(dir) // 디렉터리를 넘김 (tasks.json 경로는 내부에서 구성)
```

- [ ] **Step 8: 빌드 + 테스트**

```powershell
go build ./...
go test ./... -v 2>&1 | Select-String -Pattern "PASS|FAIL|ok"
```

Expected: 컴파일 에러 없음, 기존 테스트 통과

- [ ] **Step 9: 커밋**

```powershell
git add scheduler.go bot.go main.go
git commit -m "feat: scheduler rewrite — Task type, cron/v3, script precheck, pause/resume/update"
```

---

## Task 5: Bot !task 명령 추가

**Files:**
- Modify: `bot.go`

- [ ] **Step 1: handleCommand에 !task case 추가**

`bot.go`의 `handleCommand` switch에 추가:

```go
case "!task":
    b.handleTask(chatID, text, fields)
```

- [ ] **Step 2: handleTask 함수 추가**

```go
// handleTask processes !task commands.
// Usage:
//   !task add <cron_expr> [task] [--script <oneliner>] <prompt>
//   !task once <YYYY-MM-DD HH:MM> [task] <prompt>
//   !task list [pending|paused|all]
//   !task pause <id>
//   !task resume <id>
//   !task cancel <id>
//   !task update <id> [--cron <expr>] [--prompt <text>] [--script <script>]
func (b *Bot) handleTask(chatID int64, text string, fields []string) {
    if len(fields) < 2 {
        _ = b.Send(chatID, taskHelpText())
        return
    }
    switch fields[1] {
    case "add":
        b.handleTaskAdd(chatID, text, fields)
    case "once":
        b.handleTaskOnce(chatID, text, fields)
    case "list":
        filter := ""
        if len(fields) >= 3 {
            filter = fields[2]
        }
        b.handleTaskList(chatID, filter)
    case "pause":
        if len(fields) < 3 {
            _ = b.Send(chatID, "사용법: !task pause <id>")
            return
        }
        if err := b.scheduler.PauseTask(fields[2]); err != nil {
            _ = b.Send(chatID, "⚠️ "+err.Error())
            return
        }
        _ = b.Send(chatID, "⏸ 작업 일시정지: "+fields[2])
    case "resume":
        if len(fields) < 3 {
            _ = b.Send(chatID, "사용법: !task resume <id>")
            return
        }
        if err := b.scheduler.ResumeTask(fields[2]); err != nil {
            _ = b.Send(chatID, "⚠️ "+err.Error())
            return
        }
        _ = b.Send(chatID, "▶️ 작업 재개: "+fields[2])
    case "cancel":
        if len(fields) < 3 {
            _ = b.Send(chatID, "사용법: !task cancel <id>")
            return
        }
        if err := b.scheduler.CancelTask(fields[2]); err != nil {
            _ = b.Send(chatID, "⚠️ "+err.Error())
            return
        }
        _ = b.Send(chatID, "🗑 작업 취소: "+fields[2])
    case "update":
        b.handleTaskUpdate(chatID, fields)
    default:
        _ = b.Send(chatID, taskHelpText())
    }
}

func (b *Bot) handleTaskAdd(chatID int64, text string, fields []string) {
    // !task add <cron_expr> [task] [--script <oneliner>] <prompt>
    if len(fields) < 4 {
        _ = b.Send(chatID, "사용법: !task add <cron식> [task] [--script <스크립트>] <프롬프트>")
        return
    }
    cronExpr := fields[2]
    // Validate cron expression
    if _, err := cron.ParseStandard(cronExpr); err != nil {
        _ = b.Send(chatID, "⚠️ cron 식 오류: "+err.Error())
        return
    }

    rest := fields[3:]
    isTask := false
    script := ""

    if len(rest) > 0 && rest[0] == "task" {
        isTask = true
        rest = rest[1:]
    }
    if len(rest) >= 2 && rest[0] == "--script" {
        script = rest[1]
        rest = rest[2:]
    }
    if len(rest) == 0 {
        _ = b.Send(chatID, "⚠️ 프롬프트를 입력해주세요.")
        return
    }
    prompt := strings.Join(rest, " ")

    t := &Task{
        ChatID:   chatID,
        Prompt:   prompt,
        CronExpr: cronExpr,
        Script:   script,
        IsTask:   isTask,
        Label:    truncate(prompt, 30),
    }
    if err := b.scheduler.AddTask(t); err != nil {
        _ = b.Send(chatID, "⚠️ 작업 등록 실패: "+err.Error())
        return
    }
    kind := "알림"
    if isTask {
        kind = "Claude 작업"
    }
    _ = b.Send(chatID, fmt.Sprintf("✅ 작업 등록 [%s]\n  cron: %s\n  종류: %s\n  내용: %s",
        t.ID, cronExpr, kind, truncate(prompt, 60)))
}

func (b *Bot) handleTaskOnce(chatID int64, text string, fields []string) {
    // !task once <YYYY-MM-DD HH:MM> [task] <prompt>
    // !task once 2026-06-11 09:00 task 스크리너 실행
    if len(fields) < 5 {
        _ = b.Send(chatID, "사용법: !task once <YYYY-MM-DD> <HH:MM> [task] <프롬프트>")
        return
    }
    dateStr := fields[2] + " " + fields[3]
    fireAt, err := time.ParseInLocation("2006-01-02 15:04", dateStr, time.Local)
    if err != nil {
        _ = b.Send(chatID, "⚠️ 날짜 형식 오류 (YYYY-MM-DD HH:MM): "+err.Error())
        return
    }
    rest := fields[4:]
    isTask := false
    if len(rest) > 0 && rest[0] == "task" {
        isTask = true
        rest = rest[1:]
    }
    if len(rest) == 0 {
        _ = b.Send(chatID, "⚠️ 프롬프트를 입력해주세요.")
        return
    }
    prompt := strings.Join(rest, " ")
    t := &Task{
        ChatID: chatID,
        Prompt: prompt,
        FireAt: fireAt,
        IsTask: isTask,
        Label:  truncate(prompt, 30),
    }
    if err := b.scheduler.AddTask(t); err != nil {
        _ = b.Send(chatID, "⚠️ 작업 등록 실패: "+err.Error())
        return
    }
    _ = b.Send(chatID, fmt.Sprintf("✅ 일회성 작업 등록 [%s]\n  실행: %s\n  내용: %s",
        t.ID, fireAt.Format("2006-01-02 15:04"), truncate(prompt, 60)))
}

func (b *Bot) handleTaskList(chatID int64, filter string) {
    tasks := b.scheduler.ListTasks(filter)
    if len(tasks) == 0 {
        _ = b.Send(chatID, "등록된 작업이 없습니다.")
        return
    }
    var sb strings.Builder
    sb.WriteString(fmt.Sprintf("📋 작업 목록 (%d개):\n", len(tasks)))
    for _, t := range tasks {
        kind := "알림"
        if t.IsTask {
            kind = "Claude"
        }
        schedule := t.CronExpr
        if schedule == "" {
            schedule = t.FireAt.Format("2006-01-02 15:04")
        }
        scriptMark := ""
        if t.Script != "" {
            scriptMark = " 🔍"
        }
        fmt.Fprintf(&sb, "\n[%s] %s (%s%s)\n  📅 %s\n  %s\n",
            t.ID, t.Status, kind, scriptMark, schedule, truncate(t.Prompt, 50))
    }
    _ = b.Send(chatID, sb.String())
}

func (b *Bot) handleTaskUpdate(chatID int64, fields []string) {
    // !task update <id> [--cron <expr>] [--prompt <text>] [--script <script>]
    if len(fields) < 4 {
        _ = b.Send(chatID, "사용법: !task update <id> [--cron <식>] [--prompt <텍스트>] [--script <스크립트>]")
        return
    }
    id := fields[2]
    var u TaskUpdate
    args := fields[3:]
    for i := 0; i < len(args)-1; i++ {
        switch args[i] {
        case "--cron":
            v := args[i+1]
            u.CronExpr = &v
            i++
        case "--prompt":
            v := strings.Join(args[i+1:], " ")
            u.Prompt = &v
            i = len(args) // consume rest
        case "--script":
            v := args[i+1]
            u.Script = &v
            i++
        }
    }
    if err := b.scheduler.UpdateTask(id, u); err != nil {
        _ = b.Send(chatID, "⚠️ "+err.Error())
        return
    }
    _ = b.Send(chatID, "✅ 작업 업데이트됨: "+id)
}

func taskHelpText() string {
    return strings.TrimSpace(`
📋 !task 명령어:

!task add <cron식> [task] [--script <스크립트>] <내용>
  예) !task add "30 7 * * 1-5" task 주식 스크리너 실행
  예) !task add "0 9 * * *" 아침 알림
  예) !task add "30 7 * * 1-5" task --script "date +%u | awk '{exit ($1>=6)}' && echo '{\"wakeAgent\":true}' || echo '{\"wakeAgent\":false}'" 스크리너 실행

!task once <YYYY-MM-DD> <HH:MM> [task] <내용>
  예) !task once 2026-06-11 09:00 task 보고서 작성해줘

!task list [pending|paused|all]    목록 조회
!task pause <id>                   일시정지
!task resume <id>                  재개
!task cancel <id>                  삭제
!task update <id> --cron <식>      cron 변경
!task update <id> --prompt <텍스트> 내용 변경
!task update <id> --script <스크립트> 사전 스크립트 변경

cron 식 예: "30 7 * * 1-5" = 평일 07:30
`)
}
```

- [ ] **Step 3: helpText에 !task 추가**

`bot.go`의 `helpText()`에 다음 추가:

```go
!task add <cron식> [task] <내용>  cron 작업 등록 (예: "30 7 * * 1-5")
!task once <날짜> <시각> <내용>   일회성 작업
!task list/pause/resume/cancel/update  작업 관리
```

- [ ] **Step 4: import에 cron 추가 (bot.go)**

`bot.go` import에 `"github.com/robfig/cron/v3"` 추가.

- [ ] **Step 5: 빌드 + 테스트**

```powershell
go build ./...
go test ./... -count=1 2>&1 | Select-String "PASS|FAIL|ok"
```

- [ ] **Step 6: 커밋**

```powershell
git add bot.go
git commit -m "feat: !task command — add/once/list/pause/resume/cancel/update"
```

---

## Task 6: Telegram 첨부파일 처리

**Files:**
- Modify: `bot.go`

- [ ] **Step 1: Bot에 downloadDir 필드 추가**

`Bot` struct에 추가:

```go
type Bot struct {
    // ... 기존 필드
    attachDir string // 첨부파일 저장 디렉터리
}
```

`NewBot` 초기화 수정:

```go
func NewBot(api *tgbotapi.BotAPI, cfg *Config, store StoreRepo, manager *Manager, scheduler *Scheduler, attachDir string) *Bot {
    return &Bot{api: api, cfg: cfg, store: store, manager: manager, scheduler: scheduler, attachDir: attachDir}
}
```

- [ ] **Step 2: main.go에서 NewBot 호출 수정**

```go
attachDir := filepath.Join(dir, "attachments")
_ = os.MkdirAll(attachDir, 0o700)
bot := NewBot(api, cfg, store, manager, sched, attachDir)
```

- [ ] **Step 3: bot.go Run()에 첨부파일 처리 추가**

`Run()` 메서드의 업데이트 처리 루프를 수정:

```go
for update := range updates {
    if update.Message == nil || update.Message.From == nil {
        continue
    }
    userID := update.Message.From.ID
    if !b.cfg.IsAllowed(userID) {
        log.Printf("[bot] denied user %d (%s)", userID, update.Message.From.UserName)
        continue
    }
    chatID := update.Message.Chat.ID

    // 첨부파일 우선 처리
    if update.Message.Photo != nil || update.Message.Document != nil ||
        update.Message.Audio != nil || update.Message.Voice != nil {
        b.handleAttachment(chatID, update.Message)
        continue
    }

    text := strings.TrimSpace(update.Message.Text)
    if text == "" {
        continue
    }
    if strings.HasPrefix(text, "!") {
        b.handleCommand(chatID, text)
        continue
    }
    b.dispatchText(chatID, text)
}
```

- [ ] **Step 4: handleAttachment 함수 추가**

```go
// handleAttachment downloads a Telegram attachment and dispatches it to Claude
// with the file path embedded in the prompt.
func (b *Bot) handleAttachment(chatID int64, msg *tgbotapi.Message) {
    var fileID, ext string
    caption := strings.TrimSpace(msg.Caption)

    switch {
    case msg.Photo != nil && len(msg.Photo) > 0:
        largest := msg.Photo[len(msg.Photo)-1]
        fileID = largest.FileID
        ext = ".jpg"
    case msg.Document != nil:
        fileID = msg.Document.FileID
        ext = filepath.Ext(msg.Document.FileName)
        if ext == "" {
            ext = ".bin"
        }
    case msg.Audio != nil:
        fileID = msg.Audio.FileID
        ext = ".mp3"
    case msg.Voice != nil:
        fileID = msg.Voice.FileID
        ext = ".ogg"
    default:
        return
    }

    tgFile, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
    if err != nil {
        _ = b.Send(chatID, "⚠️ 파일 다운로드 실패: "+err.Error())
        return
    }

    url := tgFile.Link(b.api.Token)
    timestamp := time.Now().UnixMilli()
    destPath := filepath.Join(b.attachDir, fmt.Sprintf("%d%s", timestamp, ext))

    if err := downloadFile(url, destPath); err != nil {
        _ = b.Send(chatID, "⚠️ 파일 저장 실패: "+err.Error())
        return
    }

    prompt := caption
    if prompt == "" {
        prompt = "이 파일을 분석해줘"
    }
    prompt += fmt.Sprintf("\n[첨부파일: %s]", destPath)
    b.dispatchText(chatID, prompt)
}

// downloadFile saves a URL's content to a local path.
func downloadFile(url, dest string) error {
    resp, err := httpGet(url)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    f, err := os.Create(dest)
    if err != nil {
        return err
    }
    defer f.Close()
    _, err = io.Copy(f, resp.Body)
    return err
}

// httpGet is replaceable for testing.
var httpGet = func(url string) (*http.Response, error) {
    return http.Get(url) //nolint:noctx
}
```

- [ ] **Step 5: bot.go imports 추가**

```go
import (
    // 기존 imports...
    "io"
    "net/http"
)
```

- [ ] **Step 6: 빌드 확인**

```powershell
go build ./...
```

- [ ] **Step 7: 커밋**

```powershell
git add bot.go main.go
git commit -m "feat: Telegram attachment handling — photo/document/audio dispatch to Claude"
```

---

## Task 7: 대화 히스토리

**Files:**
- Create: `history.go`
- Modify: `manager.go`
- Modify: `bot.go`

- [ ] **Step 1: history.go 생성**

```go
package main

import (
    "fmt"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "time"
)

// WriteHistory appends a conversation turn to ~/.teleclaude/history/<project>/<YYYY-MM-DD>.md
func WriteHistory(project, title, prompt, response string) {
    dir, err := dataDir()
    if err != nil {
        return
    }
    histDir := filepath.Join(dir, "history", sanitizeName(project))
    if err := os.MkdirAll(histDir, 0o700); err != nil {
        return
    }
    date := time.Now().Format("2006-01-02")
    path := filepath.Join(histDir, date+".md")

    timeStr := time.Now().Format("15:04")
    entry := fmt.Sprintf("## %s — %s\n\n**요청:** %s\n\n**응답:** %s\n\n---\n\n",
        timeStr, title, prompt, truncate(response, 500))

    f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
    if err != nil {
        return
    }
    defer f.Close()
    _, _ = f.WriteString(entry)
}

// ReadHistory returns the content of a date's history file for a project.
func ReadHistory(project, date string) (string, error) {
    dir, err := dataDir()
    if err != nil {
        return "", err
    }
    path := filepath.Join(dir, "history", sanitizeName(project), date+".md")
    b, err := os.ReadFile(path)
    if err != nil {
        return "", err
    }
    return string(b), nil
}

// ListHistoryDates returns available history dates for a project, newest first.
func ListHistoryDates(project string) ([]string, error) {
    dir, err := dataDir()
    if err != nil {
        return nil, err
    }
    histDir := filepath.Join(dir, "history", sanitizeName(project))
    entries, err := os.ReadDir(histDir)
    if os.IsNotExist(err) {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    var dates []string
    for _, e := range entries {
        if strings.HasSuffix(e.Name(), ".md") {
            dates = append(dates, strings.TrimSuffix(e.Name(), ".md"))
        }
    }
    sort.Sort(sort.Reverse(sort.StringSlice(dates)))
    return dates, nil
}

// ListHistoryProjects returns all projects that have history.
func ListHistoryProjects() ([]string, error) {
    dir, err := dataDir()
    if err != nil {
        return nil, err
    }
    histBase := filepath.Join(dir, "history")
    entries, err := os.ReadDir(histBase)
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

func sanitizeName(s string) string {
    var b strings.Builder
    for _, r := range s {
        if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
            b.WriteRune('_')
        } else {
            b.WriteRune(r)
        }
    }
    return b.String()
}
```

- [ ] **Step 2: manager.go에서 Worker 완료 후 히스토리 저장**

`manager.go`의 `runWorker` 함수에서 `UpdateConversation` 호출 직전에 추가:

```go
// 히스토리 저장 (Worker 완료 시)
WriteHistory(project, workConv.Title, text, res.Text)
```

- [ ] **Step 3: bot.go에 !history 명령 추가**

`handleCommand` switch에 추가:

```go
case "!history":
    b.handleHistory(chatID, fields)
```

`handleHistory` 함수 추가:

```go
// handleHistory handles !history — retrieve conversation history.
// Usage:
//   !history                    — 저장된 프로젝트 목록
//   !history <project>          — 해당 프로젝트의 날짜 목록
//   !history <project> <date>   — 해당 날짜 히스토리 조회 (YYYY-MM-DD)
func (b *Bot) handleHistory(chatID int64, fields []string) {
    if len(fields) < 2 {
        projects, err := ListHistoryProjects()
        if err != nil || len(projects) == 0 {
            _ = b.Send(chatID, "저장된 히스토리가 없습니다.")
            return
        }
        _ = b.Send(chatID, "📚 히스토리 프로젝트:\n"+strings.Join(projects, "\n"))
        return
    }

    project := fields[1]
    if len(fields) < 3 {
        dates, err := ListHistoryDates(project)
        if err != nil || len(dates) == 0 {
            _ = b.Send(chatID, "저장된 히스토리가 없습니다: "+project)
            return
        }
        preview := dates
        if len(preview) > 10 {
            preview = preview[:10]
        }
        _ = b.Send(chatID, fmt.Sprintf("📅 %s 히스토리:\n%s", project, strings.Join(preview, "\n")))
        return
    }

    date := fields[2]
    content, err := ReadHistory(project, date)
    if err != nil {
        _ = b.Send(chatID, "히스토리를 찾을 수 없습니다: "+project+" / "+date)
        return
    }
    _ = sendChunked(b, chatID, content)
}
```

- [ ] **Step 4: helpText에 !history 추가**

```go
!history [프로젝트] [날짜]        대화 히스토리 조회
```

- [ ] **Step 5: 빌드 + 테스트**

```powershell
go build ./...
go test ./... -count=1 2>&1 | Select-String "PASS|FAIL|ok"
```

- [ ] **Step 6: 커밋**

```powershell
git add history.go manager.go bot.go
git commit -m "feat: conversation history — WriteHistory + !history command"
```

---

## Task 8: 통합 검증 + Linux 크로스컴파일

**Files:**
- Verify: 전체 빌드

- [ ] **Step 1: 전체 빌드 및 기존 테스트 통과 확인**

```powershell
go build ./...
go test ./... -count=1 -timeout 60s
```

Expected: 모든 테스트 PASS

- [ ] **Step 2: Linux ARM64 크로스컴파일**

```powershell
$env:GOOS="linux"; $env:GOARCH="arm64"
go build -o teleclaude_linux_arm64 .
$env:GOOS=""; $env:GOARCH=""
```

Expected: 에러 없음, `teleclaude_linux_arm64` 생성

- [ ] **Step 3: 바이너리를 nanopi로 전송하여 실행 확인**

```powershell
scp teleclaude_linux_arm64 nanopi:~/teleclaude_test
```

```bash
# nanopi에서
ssh nanopi "chmod +x ~/teleclaude_test && ~/teleclaude_test version"
```

Expected: `teleclaude 0.1.0` 출력

- [ ] **Step 4: nanopi에서 setup 실행 확인 (config 없을 때)**

```bash
ssh nanopi "~/teleclaude_test setup --help 2>&1 || true"
```

Expected: 에러 없이 사용법 출력

- [ ] **Step 5: 바이너리 정리**

```powershell
Remove-Item teleclaude_linux_arm64
```

- [ ] **Step 6: 커밋**

```powershell
git add -A
git commit -m "chore: cross-platform verified — Windows + Linux ARM64 builds pass"
```

---

## 검증 체크리스트

- [ ] `!task add "30 7 * * 1-5" task 주식 스크리너` → 등록 후 지정 시간에 Claude 실행
- [ ] `!task list` → 등록된 작업 목록 표시
- [ ] `!task pause <id>` / `!task resume <id>` → 상태 변경 + 다음 fire skip/재개
- [ ] `!task update <id> --cron "0 8 * * *"` → cron 변경 확인
- [ ] `!task cancel <id>` → 작업 삭제
- [ ] Script precheck: `wakeAgent: false` 반환 시 Claude 미호출
- [ ] `!remind 30m 테스트` → 30분 후 알림 도착
- [ ] `!cron add hourly task 서버 확인` → isTask=true, Claude 실행 (버그 수정 확인)
- [ ] 사진 전송 → Claude에게 파일 경로 포함 프롬프트
- [ ] `!history` → 프로젝트 목록
- [ ] `!history <project> <date>` → 해당 날짜 대화 내용
- [ ] Linux ARM64 빌드 성공
- [ ] nanopi에서 `./teleclaude_linux_arm64 version` 정상 실행
- [ ] 기존 schedule.json 자동 마이그레이션 (있는 경우)
