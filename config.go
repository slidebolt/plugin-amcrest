package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	labelHost     = "amcrest_host"
	labelUsername = "amcrest_username"
	labelPassword = "amcrest_password"
	labelScheme   = "amcrest_scheme"
	labelEvents   = "amcrest_event_codes"
)

type pluginConfig struct {
	DefaultScheme     string
	DefaultEventCodes []string
	ConnectTimeout    time.Duration
}

type deviceCredentials struct {
	Host          string             `json:"host"`
	Username      string             `json:"username"`
	Password      string             `json:"password"`
	Scheme        string             `json:"scheme"`
	EventCodes    []string           `json:"event_codes,omitempty"`
	Caps          deviceCapabilities `json:"caps,omitempty"`
	ConfigOptions []configOption     `json:"config_options,omitempty"`

	Model   string `json:"model,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Version string `json:"version,omitempty"`
}

type deviceCapabilities struct {
	Snapshot      bool `json:"snapshot"`
	StreamMain    bool `json:"stream_main"`
	DoorbellPress bool `json:"doorbell_press"`
	Motion        bool `json:"motion"`
}

type configOption struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func loadPluginConfigFromEnv() pluginConfig {
	scheme := strings.ToLower(strings.TrimSpace(os.Getenv("AMCREST_DEFAULT_SCHEME")))
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		scheme = "http"
	}

	eventCodes := splitAndDedupe(os.Getenv("AMCREST_DEFAULT_EVENT_CODES"))
	if len(eventCodes) == 0 {
		eventCodes = []string{"_DoorbellPress_", "VideoMotion"}
	}

	timeout := 3 * time.Second
	if raw := strings.TrimSpace(os.Getenv("AMCREST_CONNECT_TIMEOUT_MS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}

	return pluginConfig{
		DefaultScheme:     scheme,
		DefaultEventCodes: eventCodes,
		ConnectTimeout:    timeout,
	}
}

func splitAndDedupe(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func firstLabel(labels map[string][]string, key string) string {
	values := labels[key]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func parseDeviceCredentials(labels map[string][]string, defaults pluginConfig) deviceCredentials {
	scheme := strings.ToLower(firstLabel(labels, labelScheme))
	if scheme == "" {
		scheme = defaults.DefaultScheme
	}
	if scheme != "http" && scheme != "https" {
		scheme = defaults.DefaultScheme
	}

	eventCodes := splitAndDedupe(firstLabel(labels, labelEvents))
	if len(eventCodes) == 0 {
		eventCodes = append([]string(nil), defaults.DefaultEventCodes...)
	}

	return deviceCredentials{
		Host:       firstLabel(labels, labelHost),
		Username:   firstLabel(labels, labelUsername),
		Password:   firstLabel(labels, labelPassword),
		Scheme:     scheme,
		EventCodes: eventCodes,
	}
}

func (d deviceCredentials) valid() bool {
	if strings.TrimSpace(d.Host) == "" || strings.TrimSpace(d.Username) == "" || strings.TrimSpace(d.Password) == "" {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(d.Scheme))
	return scheme == "http" || scheme == "https"
}

func scrubSensitiveLabels(labels map[string][]string) map[string][]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string][]string, len(labels))
	for k, v := range labels {
		switch k {
		case labelHost, labelUsername, labelPassword, labelScheme, labelEvents:
			continue
		default:
			out[k] = append([]string(nil), v...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
