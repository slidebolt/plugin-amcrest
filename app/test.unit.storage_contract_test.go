package app_test

import (
	"encoding/json"
	"reflect"
	"testing"

	amcrestapp "github.com/slidebolt/plugin-amcrest/app"
	domain "github.com/slidebolt/sb-domain"
	testkit "github.com/slidebolt/sb-testkit"
)

func TestStorageContract_CameraEntityRoundTrips(t *testing.T) {
	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")

	entity := domain.Entity{
		ID:       "front_door",
		Plugin:   amcrestapp.PluginID,
		DeviceID: "front_door",
		Type:     "camera",
		Name:     "Front Door",
		Commands: []string{"camera_snapshot"},
		State: domain.Camera{
			MotionDetection: true,
		},
		Meta: map[string]json.RawMessage{
			"connected":   json.RawMessage(`true`),
			"version":     json.RawMessage(`"1.2.3"`),
			"event_codes": json.RawMessage(`["VideoMotion"]`),
		},
	}
	if err := env.Storage().Save(entity); err != nil {
		t.Fatalf("save entity: %v", err)
	}

	raw, err := env.Storage().Get(domain.EntityKey{Plugin: amcrestapp.PluginID, DeviceID: "front_door", ID: "front_door"})
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	var got domain.Entity
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Commands, []string{"camera_snapshot"}) {
		t.Fatalf("commands = %v", got.Commands)
	}
	state, ok := got.State.(domain.Camera)
	if !ok {
		t.Fatalf("state type = %T", got.State)
	}
	if !state.MotionDetection {
		t.Fatalf("state = %+v", state)
	}
}
