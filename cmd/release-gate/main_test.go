package main

import (
	"context"
	"testing"
	"time"
)

func env(addr string) func(string) string {
	return func(key string) string {
		if key == "ADDR" {
			return addr
		}
		return ""
	}
}

func TestStartRejectsInvalidAddress(t *testing.T) {
	if code := start(t.Context(), env("not-an-address")); code != 1 {
		t.Fatalf("start with an invalid ADDR = %d, want 1", code)
	}
}

func TestStartExitsCleanlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if code := start(ctx, env("127.0.0.1:0")); code != 0 {
		t.Fatalf("start after a clean shutdown = %d, want 0", code)
	}
}
