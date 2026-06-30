package main

import (
	"sync"
	"testing"
)

func TestConfigHolder_GetSet(t *testing.T) {
	h := NewConfigHolder(&Config{WorkerModel: "a"})
	if h.Get().WorkerModel != "a" {
		t.Fatal("initial get")
	}
	h.Set(&Config{WorkerModel: "b"})
	if h.Get().WorkerModel != "b" {
		t.Fatal("after set")
	}
}

func TestConfigHolder_ConcurrentRace(t *testing.T) {
	h := NewConfigHolder(&Config{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = h.Get() }()
		go func() { defer wg.Done(); h.Set(&Config{MaxWorkers: 1}) }()
	}
	wg.Wait()
}

func TestRateLimiter_SetLimit(t *testing.T) {
	r := NewRateLimiter(1)
	if !r.Allow(7) {
		t.Fatal("first allowed")
	}
	if r.Allow(7) {
		t.Fatal("second should be blocked at limit 1")
	}
	r.SetLimit(0) // unlimited
	if !r.Allow(7) {
		t.Fatal("after SetLimit(0) should be unlimited")
	}
}
