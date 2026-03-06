package main

import (
	"testing"
	"time"
)

func TestLoadPluginConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("AMCREST_DEFAULT_SCHEME", "")
	t.Setenv("AMCREST_DEFAULT_EVENT_CODES", "")
	t.Setenv("AMCREST_CONNECT_TIMEOUT_MS", "")

	cfg := loadPluginConfigFromEnv()
	if cfg.DefaultScheme != "http" {
		t.Fatalf("expected default scheme http, got %q", cfg.DefaultScheme)
	}
	if len(cfg.DefaultEventCodes) != 2 || cfg.DefaultEventCodes[0] != "_DoorbellPress_" || cfg.DefaultEventCodes[1] != "VideoMotion" {
		t.Fatalf("unexpected default event codes: %#v", cfg.DefaultEventCodes)
	}
	if cfg.ConnectTimeout != 3*time.Second {
		t.Fatalf("unexpected default timeout: %v", cfg.ConnectTimeout)
	}
}

func TestParseDeviceCredentialsFromLabels(t *testing.T) {
	cfg := pluginConfig{DefaultScheme: "http", DefaultEventCodes: []string{"_DoorbellPress_"}}
	labels := map[string][]string{
		labelHost:     {"10.0.0.10"},
		labelUsername: {"admin"},
		labelPassword: {"secret"},
		labelScheme:   {"https"},
		labelEvents:   {"_DoorbellPress_,VideoMotion"},
	}
	creds := parseDeviceCredentials(labels, cfg)
	if !creds.valid() {
		t.Fatal("expected valid credentials")
	}
	if creds.Host != "10.0.0.10" || creds.Username != "admin" || creds.Password != "secret" {
		t.Fatalf("unexpected credential parse: %#v", creds)
	}
	if creds.Scheme != "https" {
		t.Fatalf("expected https scheme, got %q", creds.Scheme)
	}
	if len(creds.EventCodes) != 2 {
		t.Fatalf("unexpected event codes: %#v", creds.EventCodes)
	}
}

func TestScrubSensitiveLabels(t *testing.T) {
	in := map[string][]string{
		labelHost:     {"10.0.0.10"},
		labelUsername: {"admin"},
		labelPassword: {"secret"},
		"room":        {"front-door"},
	}
	out := scrubSensitiveLabels(in)
	if out[labelHost] != nil || out[labelUsername] != nil || out[labelPassword] != nil {
		t.Fatalf("sensitive labels were not removed: %#v", out)
	}
	if got := out["room"]; len(got) != 1 || got[0] != "front-door" {
		t.Fatalf("non-sensitive labels should remain, got %#v", out)
	}
}
