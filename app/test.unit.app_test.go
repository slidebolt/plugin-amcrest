package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	domain "github.com/slidebolt/sb-domain"
)

func TestPluginHello(t *testing.T) {
	hello := New().Hello()
	if hello.ID != PluginID {
		t.Errorf("ID: got %q, want %q", hello.ID, PluginID)
	}
	if hello.Kind != "plugin" {
		t.Errorf("Kind: got %q, want %q", hello.Kind, "plugin")
	}
	if len(hello.DependsOn) != 2 {
		t.Errorf("DependsOn: got %v, want [messenger storage]", hello.DependsOn)
	}
}

func TestDesiredEntitiesAreEventCentric(t *testing.T) {
	app := New()
	device := domain.Device{ID: "front-door", Plugin: PluginID, Name: "Front Door"}
	status := DeviceStatus{
		Connected:       true,
		Version:         "1.2.3",
		EventCodes:      []string{"VideoMotion", "_DoorBell_"},
		LastHeartbeatAt: "2026-04-12T00:00:00Z",
		LastEventAt:     "2026-04-12T00:01:00Z",
		LastEventCode:   "_DoorBell_",
		LastEventAction: "Start",
		LastEventRaw:    "Code=_DoorBell_;action=Start;index=0",
		ActiveEvents: map[string]bool{
			"_DoorBell_": true,
		},
		EventCounts: map[string]int{
			"_DoorBell_":  2,
			"VideoMotion": 4,
		},
	}

	entities := app.desiredEntities(device, status)
	byID := map[string]domain.Entity{}
	for _, entity := range entities {
		byID[entity.ID] = entity
	}

	if _, ok := byID["front-door"]; ok {
		t.Fatal("unexpected legacy root entity")
	}
	if _, ok := byID["connection"]; !ok {
		t.Fatal("missing connection entity")
	}
	if _, ok := byID["supported-event-codes"]; !ok {
		t.Fatal("missing supported-event-codes entity")
	}
	doorbell, ok := byID["event-doorbell"]
	if !ok {
		t.Fatal("missing generic doorbell event entity")
	}
	if doorbell.Type != "binary_sensor" {
		t.Fatalf("event-doorbell type = %q", doorbell.Type)
	}
	doorbellState, ok := doorbell.State.(domain.BinarySensor)
	if !ok {
		t.Fatalf("event-doorbell state type = %T", doorbell.State)
	}
	if !doorbellState.On {
		t.Fatalf("event-doorbell should be active, got %+v", doorbellState)
	}
	countEntity, ok := byID["event-doorbell-count"]
	if !ok {
		t.Fatal("missing doorbell count entity")
	}
	countState, ok := countEntity.State.(domain.Sensor)
	if !ok {
		t.Fatalf("event-doorbell-count state type = %T", countEntity.State)
	}
	if countState.Value != 2 {
		t.Fatalf("event-doorbell-count = %#v, want 2", countState.Value)
	}
}

func TestApplyEventUpdatesHeartbeatAndCounts(t *testing.T) {
	app := New()

	status := app.applyEvent("front-door", Event{Code: "Heartbeat", Raw: "Heartbeat"})
	if status.LastHeartbeatAt == "" {
		t.Fatal("expected heartbeat timestamp to be set")
	}

	status = app.applyEvent("front-door", Event{Code: "_DoorBell_", Action: "Start", Raw: "Code=_DoorBell_;action=Start"})
	if status.LastEventCode != "_DoorBell_" {
		t.Fatalf("LastEventCode = %q", status.LastEventCode)
	}
	if !status.ActiveEvents["_DoorBell_"] {
		t.Fatalf("doorbell event should be active")
	}
	if status.EventCounts["_DoorBell_"] != 1 {
		t.Fatalf("doorbell count = %d", status.EventCounts["_DoorBell_"])
	}

	status = app.applyEvent("front-door", Event{Code: "_DoorBell_", Action: "Stop", Raw: "Code=_DoorBell_;action=Stop"})
	if status.ActiveEvents["_DoorBell_"] {
		t.Fatalf("doorbell event should have been cleared")
	}
	if status.EventCounts["_DoorBell_"] != 2 {
		t.Fatalf("doorbell count after stop = %d", status.EventCounts["_DoorBell_"])
	}
}

func TestAmcrestClientNew(t *testing.T) {
	client := NewAmcrestClient("http://192.168.1.100", "admin", "password", 10*time.Second)
	if client.BaseURL != "http://192.168.1.100" {
		t.Errorf("BaseURL: got %q, want %q", client.BaseURL, "http://192.168.1.100")
	}
	if client.Username != "admin" {
		t.Errorf("Username: got %q, want %q", client.Username, "admin")
	}
	if client.Password != "password" {
		t.Errorf("Password: got %q, want %q", client.Password, "password")
	}
}

func TestParseKV(t *testing.T) {
	in := strings.NewReader("a=1\nb=two\n")
	got := parseKV(in)
	if got["a"] != "1" || got["b"] != "two" {
		t.Fatalf("unexpected kv: %+v", got)
	}
}

func TestParseEventLine(t *testing.T) {
	ev := parseEventLine("Code=_DoorBell_;action=Start;index=0;data=pressed")
	if ev.Code != "_DoorBell_" || ev.Action != "Start" || ev.Index != "0" || ev.Data != "pressed" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestStorageRoundTripForGenericEventEntities(t *testing.T) {
	entity := domain.Entity{
		ID:       "event-doorbell-count",
		Plugin:   PluginID,
		DeviceID: "front-door",
		Type:     "sensor",
		Name:     "DoorBell Count",
		State:    domain.Sensor{Value: 3},
	}
	data, err := json.Marshal(entity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got domain.Entity
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	state, ok := got.State.(domain.Sensor)
	if !ok {
		t.Fatalf("state type = %T", got.State)
	}
	if state.Value != float64(3) && state.Value != 3 {
		t.Fatalf("state value = %#v", state.Value)
	}
}
