//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	app "github.com/slidebolt/plugin-amcrest/app"
	domain "github.com/slidebolt/sb-domain"
	managersdk "github.com/slidebolt/sb-manager-sdk"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
)

func loadEnvLocal(t *testing.T) map[string]string {
	t.Helper()

	f, err := os.Open("../../.env.local")
	if err != nil {
		t.Fatalf("open .env.local: %v", err)
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid env line %q", line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan .env.local: %v", err)
	}
	return out
}

func saveProvisionedCamera(t *testing.T, store storage.Storage, env map[string]string) {
	t.Helper()

	timeout := json.RawMessage("3000")
	if v := env["AMCREST_TIMEOUT_MS"]; v != "" {
		timeout = json.RawMessage(v)
	}
	port := json.RawMessage("80")
	if v := env["AMCREST_PORT"]; v != "" {
		port = json.RawMessage(v)
	}
	scheme := env["AMCREST_SCHEME"]
	if scheme == "" {
		scheme = "http"
	}

	device := domain.Device{
		ID:     "default",
		Plugin: app.PluginID,
		Name:   "Amcrest Test Camera",
		Meta: map[string]json.RawMessage{
			"host":       json.RawMessage(`"` + env["AMCREST_HOST"] + `"`),
			"username":   json.RawMessage(`"` + env["AMCREST_USERNAME"] + `"`),
			"password":   json.RawMessage(`"` + env["AMCREST_PASSWORD"] + `"`),
			"scheme":     json.RawMessage(`"` + scheme + `"`),
			"port":       port,
			"timeout_ms": timeout,
		},
	}
	if err := store.Save(device); err != nil {
		t.Fatalf("save device: %v", err)
	}

	entity := domain.Entity{
		ID:       "default",
		Plugin:   app.PluginID,
		DeviceID: "default",
		Type:     "camera",
		Name:     "Amcrest Test Camera",
		Commands: []string{"camera_snapshot"},
		State:    domain.Camera{},
		Meta: map[string]json.RawMessage{
			"last_error": json.RawMessage(`"pending"`),
		},
	}
	if err := store.Save(entity); err != nil {
		t.Fatalf("save entity: %v", err)
	}
}

// TestDiscovery_FindCameras provisions a real Amcrest camera through storage,
// starts the plugin, and verifies the plugin reconciles and can snapshot it.
func TestDiscovery_FindCameras(t *testing.T) {
	envVars := loadEnvLocal(t)
	for _, key := range []string{"AMCREST_HOST", "AMCREST_USERNAME", "AMCREST_PASSWORD"} {
		if envVars[key] == "" {
			t.Fatalf("%s not set in .env.local", key)
		}
	}

	env := managersdk.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")
	saveProvisionedCamera(t, env.Storage(), envVars)

	p := app.New()
	if _, err := p.OnStart(map[string]json.RawMessage{
		"messenger": env.MessengerPayload(),
	}); err != nil {
		t.Fatalf("OnStart: %v", err)
	}
	defer p.OnShutdown()

	raw, err := env.Storage().Get(domain.EntityKey{Plugin: app.PluginID, DeviceID: "default", ID: "default"})
	if err != nil {
		t.Fatalf("get camera entity: %v", err)
	}
	var camera domain.Entity
	if err := json.Unmarshal(raw, &camera); err != nil {
		t.Fatalf("unmarshal camera entity: %v", err)
	}
	// Verify connected via meta
	var connected bool
	if raw := camera.Meta["connected"]; len(raw) > 0 {
		json.Unmarshal(raw, &connected)
	}
	if !connected {
		t.Fatalf("expected connected camera, got meta %v", camera.Meta)
	}

	cmds := messenger.NewCommands(env.Messenger(), domain.LookupCommand)
	if err := cmds.Send(camera, app.CameraSnapshot{}); err != nil {
		t.Fatalf("send snapshot command: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := env.Storage().Get(domain.EntityKey{Plugin: app.PluginID, DeviceID: "default", ID: "default"})
		if err != nil {
			t.Fatalf("get updated camera entity: %v", err)
		}
		if err := json.Unmarshal(raw, &camera); err != nil {
			t.Fatalf("unmarshal updated camera entity: %v", err)
		}
		var lastErr string
		if raw := camera.Meta["last_error"]; len(raw) > 0 {
			json.Unmarshal(raw, &lastErr)
		}
		if lastErr == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for successful snapshot, last_error=%q", lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
