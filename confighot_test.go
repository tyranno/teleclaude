package main

import "testing"

func TestApplyReload_RateLimitChanged(t *testing.T) {
	old := &Config{RateLimitPerMin: 20, TelegramBotToken: "t"}
	nw := &Config{RateLimitPerMin: 5, TelegramBotToken: "t"}
	var gotLimit = -999
	applyReload(old, nw, ReloadHooks{OnRateLimit: func(n int) { gotLimit = n }})
	if gotLimit != 5 {
		t.Errorf("OnRateLimit got %d, want 5", gotLimit)
	}
}

func TestApplyReload_TokenChanged(t *testing.T) {
	old := &Config{TelegramBotToken: "A"}
	nw := &Config{TelegramBotToken: "B"}
	called := false
	applyReload(old, nw, ReloadHooks{OnTokenChanged: func() { called = true }})
	if !called {
		t.Error("OnTokenChanged should fire on token change")
	}
}

func TestApplyReload_ScreenControlToggle(t *testing.T) {
	old := &Config{ScreenControl: false}
	nw := &Config{ScreenControl: true}
	var got *bool
	applyReload(old, nw, ReloadHooks{OnScreenControl: func(b bool) { got = &b }})
	if got == nil || *got != true {
		t.Error("OnScreenControl should fire true")
	}
}

func TestApplyReload_NoChange_NoHooks(t *testing.T) {
	c := &Config{TelegramBotToken: "t", RateLimitPerMin: 20}
	applyReload(c, &Config{TelegramBotToken: "t", RateLimitPerMin: 20}, ReloadHooks{
		OnRateLimit:    func(int) { t.Error("rate hook should not fire") },
		OnTokenChanged: func() { t.Error("token hook should not fire") },
	})
}
