//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	app "github.com/slidebolt/plugin-amcrest/app"
	domain "github.com/slidebolt/sb-domain"
	storage "github.com/slidebolt/sb-storage-sdk"
	testkit "github.com/slidebolt/sb-testkit"
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

func saveProvisionedDevice(t *testing.T, store storage.Storage, env map[string]string) {
	t.Helper()

	timeoutMs := 3000
	if v := env["AMCREST_TIMEOUT_MS"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			timeoutMs = n
		}
	}
	port := 80
	if v := env["AMCREST_PORT"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	scheme := env["AMCREST_SCHEME"]
	if scheme == "" {
		scheme = "http"
	}

	creds := app.CameraConfig{
		Host:     env["AMCREST_HOST"],
		Username: env["AMCREST_USERNAME"],
		Password: env["AMCREST_PASSWORD"],
		Scheme:   scheme,
		Port:     port,
		Timeout:  timeoutMs,
	}
	privateJSON, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}

	device := domain.Device{
		ID:     "default",
		Plugin: app.PluginID,
		Name:   "Amcrest Test Device",
	}
	if err := store.Save(device); err != nil {
		t.Fatalf("save device: %v", err)
	}
	if err := store.SetPrivate(device, json.RawMessage(privateJSON)); err != nil {
		t.Fatalf("set private: %v", err)
	}
}

// TestDiscovery_FindCameras provisions a real Amcrest device through storage,
// starts the plugin, and verifies the plugin reconciles event-oriented entities.
func TestDiscovery_FindCameras(t *testing.T) {
	envVars := loadEnvLocal(t)
	for _, key := range []string{"AMCREST_HOST", "AMCREST_USERNAME", "AMCREST_PASSWORD"} {
		if envVars[key] == "" {
			t.Fatalf("%s not set in .env.local", key)
		}
	}

	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")
	saveProvisionedDevice(t, env.Storage(), envVars)

	p := app.New()
	if _, err := p.OnStart(map[string]json.RawMessage{
		"messenger": env.MessengerPayload(),
	}); err != nil {
		t.Fatalf("OnStart: %v", err)
	}
	defer p.OnShutdown()

	status, ok := p.Status("default")
	if !ok {
		t.Fatalf("no status recorded for camera default")
	}
	if !status.Connected {
		t.Fatalf("expected connected device, got status %+v", status)
	}
	t.Logf("device connected: version=%q event_codes=%v", status.Version, status.EventCodes)

	raw, err := env.Storage().Get(domain.EntityKey{Plugin: app.PluginID, DeviceID: "default", ID: "connection"})
	if err != nil {
		t.Fatalf("get connection entity: %v", err)
	}
	var connection domain.Entity
	if err := json.Unmarshal(raw, &connection); err != nil {
		t.Fatalf("unmarshal connection entity: %v", err)
	}
	if connection.Type != "diagnostic" {
		t.Fatalf("connection type = %q, want diagnostic", connection.Type)
	}

	raw, err = env.Storage().Get(domain.EntityKey{Plugin: app.PluginID, DeviceID: "default", ID: "supported-event-codes"})
	if err != nil {
		t.Fatalf("get supported-event-codes entity: %v", err)
	}
	var codes domain.Entity
	if err := json.Unmarshal(raw, &codes); err != nil {
		t.Fatalf("unmarshal supported-event-codes entity: %v", err)
	}
	t.Logf("supported-event-codes entity present: %s", codes.Key())
}
