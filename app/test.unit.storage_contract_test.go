package app_test

import (
	"encoding/json"
	"reflect"
	"testing"

	amcrestapp "github.com/slidebolt/plugin-amcrest/app"
	domain "github.com/slidebolt/sb-domain"
	testkit "github.com/slidebolt/sb-testkit"
)

func TestStorageContract_EventEntitiesRoundTrip(t *testing.T) {
	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")

	connection := domain.Entity{
		ID:       "connection",
		Plugin:   amcrestapp.PluginID,
		DeviceID: "front_door",
		Type:     "diagnostic",
		Name:     "Amcrest Event Connection",
		State: map[string]any{
			"connected":       true,
			"lastEventCode":   "_DoorBell_",
			"lastHeartbeatAt": "2026-04-12T00:00:00Z",
		},
	}
	if err := env.Storage().Save(connection); err != nil {
		t.Fatalf("save connection: %v", err)
	}

	counter := domain.Entity{
		ID:       "event-doorbell-count",
		Plugin:   amcrestapp.PluginID,
		DeviceID: "front_door",
		Type:     "sensor",
		Name:     "DoorBell Count",
		State:    domain.Sensor{Value: 2},
	}
	if err := env.Storage().Save(counter); err != nil {
		t.Fatalf("save counter: %v", err)
	}

	raw, err := env.Storage().Get(domain.EntityKey{Plugin: amcrestapp.PluginID, DeviceID: "front_door", ID: "event-doorbell-count"})
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	var got domain.Entity
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Commands, []string(nil)) {
		t.Fatalf("commands = %v, want nil", got.Commands)
	}
	state, ok := got.State.(domain.Sensor)
	if !ok {
		t.Fatalf("state type = %T", got.State)
	}
	if state.Value != float64(2) && state.Value != 2 {
		t.Fatalf("state = %+v", state)
	}
}
