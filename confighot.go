package main

import (
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ReloadHooks are invoked by applyReload when specific fields change.
type ReloadHooks struct {
	OnRateLimit     func(int)  // new rate limit
	OnTokenChanged  func()     // bot token changed (needs restart)
	OnScreenControl func(bool) // screen_control.enabled toggled
	Notify          func(string)
}

// applyReload compares old vs new config and fires the relevant hooks.
func applyReload(old, nw *Config, h ReloadHooks) {
	if old.RateLimitPerMin != nw.RateLimitPerMin && h.OnRateLimit != nil {
		h.OnRateLimit(nw.RateLimitPerMin)
	}
	if old.TelegramBotToken != nw.TelegramBotToken && h.OnTokenChanged != nil {
		h.OnTokenChanged()
	}
	if old.ScreenControl != nw.ScreenControl && h.OnScreenControl != nil {
		h.OnScreenControl(nw.ScreenControl)
	}
}

// WatchConfig watches the config file's directory and hot-reloads on change.
// Returns a stop func. Editor atomic-saves (temp+rename) are handled by watching
// the directory and filtering for the config file name; events are debounced.
func WatchConfig(path string, holder *ConfigHolder, hooks ReloadHooks) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	done := make(chan struct{})
	go func() {
		var timer *time.Timer
		reload := func() {
			cfg, err := LoadConfig(path)
			if err != nil {
				log.Printf("[config] reload 실패: %v (이전 설정 유지)", err)
				if hooks.Notify != nil {
					hooks.Notify("⚠️ 설정 reload 실패: " + err.Error() + " — 이전 설정 유지")
				}
				return
			}
			old := holder.Get()
			holder.Set(cfg)
			applyReload(old, cfg, hooks)
			log.Printf("[config] reload 적용됨")
			if hooks.Notify != nil {
				hooks.Notify("⚙️ 설정이 reload되었습니다")
			}
		}
		for {
			select {
			case <-done:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != name {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(300*time.Millisecond, reload)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[config] watcher 오류: %v", err)
			}
		}
	}()

	return func() { close(done); _ = w.Close() }, nil
}
