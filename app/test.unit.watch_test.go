package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	domain "github.com/slidebolt/sb-domain"
	testkit "github.com/slidebolt/sb-testkit"
)

func TestOnStartPicksUpProvisionedDeviceWithoutRestart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="amcrest", nonce="abc123", qop="auth", opaque="opaque123"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/cgi-bin/magicBox.cgi":
			switch r.URL.Query().Get("action") {
			case "getSystemInfo":
				fmt.Fprintln(w, "deviceType=Doorbell")
			case "getSoftwareVersion":
				fmt.Fprintln(w, "version=1.2.3")
			default:
				http.NotFound(w, r)
			}
		case "/cgi-bin/eventManager.cgi":
			switch r.URL.Query().Get("action") {
			case "getEventIndexes":
				fmt.Fprintln(w, "events[0]=_DoorBell_")
				fmt.Fprintln(w, "events[1]=VideoMotion")
			case "attach":
				w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
				fmt.Fprint(w, "--myboundary\r\nContent-Type: text/plain\r\nContent-Length: 9\r\n\r\nHeartbeat\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")

	app := New()
	if _, err := app.OnStart(map[string]json.RawMessage{
		"messenger": env.MessengerPayload(),
	}); err != nil {
		t.Fatalf("OnStart: %v", err)
	}
	defer app.OnShutdown()

	privateJSON, err := json.Marshal(CameraConfig{
		Host:     parsed.Hostname(),
		Port:     port,
		Username: "admin",
		Password: "secret",
		Scheme:   parsed.Scheme,
		Timeout:  3000,
	})
	if err != nil {
		t.Fatalf("marshal private config: %v", err)
	}

	device := domain.Device{
		ID:      "doorbell",
		Plugin:  PluginID,
		Name:    "Front Door",
		Private: json.RawMessage(privateJSON),
	}
	if err := env.Storage().Save(device); err != nil {
		t.Fatalf("save provisioned device: %v", err)
	}

	var codes domain.Entity
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := env.Storage().Get(domain.EntityKey{Plugin: PluginID, DeviceID: "doorbell", ID: "supported-event-codes"})
		if err == nil {
			if err := json.Unmarshal(raw, &codes); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for discovered entities")
		}
		time.Sleep(50 * time.Millisecond)
	}

	state, ok := codes.State.(domain.Sensor)
	if !ok {
		t.Fatalf("supported-event-codes state type = %T", codes.State)
	}
	values, ok := state.Value.([]any)
	if !ok || len(values) != 2 {
		t.Fatalf("supported-event-codes value = %#v", state.Value)
	}

	status, ok := app.Status("doorbell")
	if !ok {
		t.Fatal("missing runtime status for doorbell")
	}
	if !status.Connected {
		t.Fatalf("expected connected status, got %+v", status)
	}
}
