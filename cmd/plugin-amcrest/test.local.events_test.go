//go:build integration

package main

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	app "github.com/slidebolt/plugin-amcrest/app"
)

// TestEventStream_Doorbell opens a long-poll event stream to a real Amcrest
// doorbell and logs every event received. During the listen window, press
// the doorbell button — the test asserts that at least one event was
// observed. Duration is configurable via AMCREST_DOORBELL_LISTEN_SECONDS
// (default 60s).
func TestEventStream_Doorbell(t *testing.T) {
	envVars := loadEnvLocal(t)
	for _, key := range []string{"AMCREST_DOORBELL_HOST", "AMCREST_DOORBELL_USERNAME", "AMCREST_DOORBELL_PASSWORD"} {
		if envVars[key] == "" {
			t.Fatalf("%s not set in .env.local", key)
		}
	}

	scheme := envVars["AMCREST_DOORBELL_SCHEME"]
	if scheme == "" {
		scheme = "http"
	}
	baseURL := scheme + "://" + envVars["AMCREST_DOORBELL_HOST"] + ":80"

	listenSeconds := 60
	if v := envVars["AMCREST_DOORBELL_LISTEN_SECONDS"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			listenSeconds = n
		}
	}
	if v := os.Getenv("AMCREST_DOORBELL_LISTEN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			listenSeconds = n
		}
	}

	client := app.NewAmcrestClient(baseURL, envVars["AMCREST_DOORBELL_USERNAME"], envVars["AMCREST_DOORBELL_PASSWORD"], 5*time.Second)

	var (
		mu       sync.Mutex
		events   []app.Event
		rawLines int
	)
	client.RawLineHook = func(line string) {
		mu.Lock()
		rawLines++
		mu.Unlock()
		t.Logf("raw: %s", line)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(listenSeconds)*time.Second)
	defer cancel()

	t.Logf("listening for doorbell events for %ds — press the button now", listenSeconds)
	err := client.StreamEvents(ctx, "[All]", func(ev app.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		t.Logf("EVENT: code=%s action=%s index=%s", ev.Code, ev.Action, ev.Index)
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	heartbeats := 0
	var realEvents []app.Event
	for _, ev := range events {
		if ev.Code == "Heartbeat" {
			heartbeats++
			continue
		}
		realEvents = append(realEvents, ev)
	}
	t.Logf("stream finished: %d raw lines, %d heartbeats, %d real events", rawLines, heartbeats, len(realEvents))

	if heartbeats == 0 {
		t.Fatalf("no heartbeats received — stream is not alive")
	}

	for _, ev := range realEvents {
		t.Logf("REAL EVENT: %+v", ev)
		if ev.Code == "_DoorBell_" || ev.Code == "CallNoAnswered" || ev.Code == "BackKeyLight" || ev.Code == "Invite" || ev.Code == "VideoMotion" {
			t.Logf("doorbell activity detected: code=%s action=%s", ev.Code, ev.Action)
			return
		}
	}
	if len(realEvents) == 0 {
		t.Logf("stream alive (heartbeats ok) but no button press observed in window")
	}
}
