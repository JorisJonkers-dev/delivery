package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// unreachable is a cluster nothing answers at: every read the gate makes of it fails.
func unreachable() *rest.Config { return &rest.Config{Host: "http://127.0.0.1:1"} }

func env(addr string) func(string) string {
	return func(key string) string {
		if key == "ADDR" {
			return addr
		}
		return ""
	}
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestStartRejectsInvalidAddress(t *testing.T) {
	if code := start(t.Context(), quiet(), env("not-an-address"), unreachable()); code != 1 {
		t.Fatalf("start with an invalid ADDR = %d, want 1", code)
	}
}

func TestStartRejectsAClusterConfigurationThatIsNotOne(t *testing.T) {
	var logged bytes.Buffer
	config := &rest.Config{Host: "http://127.0.0.1:1", TLSClientConfig: rest.TLSClientConfig{CertFile: "/nowhere/cert", KeyFile: "/nowhere/key"}}
	code := start(t.Context(), slog.New(slog.NewJSONHandler(&logged, nil)), env("127.0.0.1:0"), config)
	if code != 1 || !strings.Contains(logged.String(), "reach the cluster") {
		t.Fatalf("exit %d, log %s", code, logged.String())
	}
}

func TestStartExitsCleanlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if code := start(ctx, quiet(), env("127.0.0.1:0"), unreachable()); code != 0 {
		t.Fatalf("start after a clean shutdown = %d, want 0", code)
	}
}

func TestTheBinaryAnswersTheGatesWebhooks_andNoWhenItCannotReadTheCluster(t *testing.T) {
	// A free port, so the test knows where the binary listens.
	var lc net.ListenConfig
	probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- start(ctx, quiet(), env(addr), unreachable()) }()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("the binary stopped with %d", code)
		}
	})

	body := `{"name":"notes-api","namespace":"notes-system","phase":"Progressing","metadata":{"application":"notes","process":"notes-api","revision":"sha256:9d2c4e6a8b0d1f3a5c7e9b1d3f5a7c9e"}}`
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/may-promote", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("may-promote with no cluster to read = %d, want 503", resp.StatusCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the binary never listened: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
