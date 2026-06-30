---
name: project-overview
description: teleclaude Go CLI structure and Phase 0 config-yaml/hot-reload design facts relevant to code review
metadata:
  type: project
---

teleclaude is a single-package Go CLI (`package main`) bridging Telegram and Claude/Codex CLIs on Windows.

Phase 0 (config.yaml + hot-reload) key components:
- `ConfigHolder` (confighold.go): `atomic.Pointer[Config]` for lock-free config reads. `Get()`/`Set()`.
- `WatchConfig` (confighot.go): fsnotify directory watcher + 300ms debounce timer, returns stop func that `close(done)` + `w.Close()`.
- `RateLimiter` (security.go): per-user sliding window, `SetLimit` for live updates. NOTE: `Remaining()` reads `maxPerMin` outside the lock (data race).
- `LoadOrMigrate` (config.go): migrates config.txt → config.yaml, renames txt to .bak.
- Hooks wired in main.go run(): OnRateLimit → rateLimiter.SetLimit; OnTokenChanged needs restart; Notify broadcasts to AllowedUserIDs.

**Why:** Phase 0 introduced live config reload, replacing static `*Config` passing with a holder.
**How to apply:** When reviewing concurrency, check the debounce timer is only touched by the single watcher goroutine (it is — safe), but RateLimiter shares state across the bot's many goroutines. codexRunner (runner_codex.go) captures `cfg *Config` at construction and does NOT use the holder — it won't see reloaded config.
