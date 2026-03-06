package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/slidebolt/plugin-amcrest/pkg/amcrest"
	runner "github.com/slidebolt/sdk-runner"
	"github.com/slidebolt/sdk-types"
)

type fakeClient struct {
	info        map[string]string
	version     string
	snapshot    []byte
	systemErr   error
	snapshotErr error
	eventCodes  []string
	allConfig   map[string]string
}

func (f *fakeClient) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	if f.systemErr != nil {
		return nil, f.systemErr
	}
	return f.info, nil
}
func (f *fakeClient) GetSoftwareVersion(ctx context.Context) (string, error) { return f.version, nil }
func (f *fakeClient) GetSnapshot(ctx context.Context) ([]byte, error) {
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	return append([]byte(nil), f.snapshot...), nil
}
func (f *fakeClient) GetEventCodes(ctx context.Context) ([]string, error) {
	return append([]string(nil), f.eventCodes...), nil
}
func (f *fakeClient) GetAllConfig(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(f.allConfig))
	for k, v := range f.allConfig {
		out[k] = v
	}
	return out, nil
}
func (f *fakeClient) SubscribeEvents(ctx context.Context, codes []string, onEvent func(amcrest.Event)) error {
	return nil
}

type fakeSink struct{ events []types.InboundEvent }

func (s *fakeSink) EmitEvent(evt types.InboundEvent) error {
	s.events = append(s.events, evt)
	return nil
}

type mockRawStore struct {
	devices map[string]json.RawMessage
}

func newMockRawStore() *mockRawStore { return &mockRawStore{devices: map[string]json.RawMessage{}} }

func (m *mockRawStore) ReadRawDevice(deviceID string) (json.RawMessage, error) {
	if v, ok := m.devices[deviceID]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (m *mockRawStore) WriteRawDevice(deviceID string, data json.RawMessage) error {
	m.devices[deviceID] = append([]byte(nil), data...)
	return nil
}
func (m *mockRawStore) ReadRawEntity(deviceID, entityID string) (json.RawMessage, error) {
	return nil, errors.New("not implemented")
}
func (m *mockRawStore) WriteRawEntity(deviceID, entityID string, data json.RawMessage) error {
	return nil
}

func TestOnDeviceCreateStoresRawCredentialsAndScrubsLabels(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			info:       map[string]string{"deviceType": "AD410", "serialNumber": "SER123"},
			version:    "1.0.0",
			snapshot:   []byte{1, 2},
			eventCodes: []string{"_DoorbellPress_", "VideoMotion"},
			allConfig: map[string]string{
				"table.All.MotionDetect[0].Enable": "false",
				"table.All.Email.Enable":           "true",
			},
		}
	}

	dev, err := p.OnDeviceCreate(types.Device{
		LocalName: "Front Door",
		Labels: map[string][]string{
			labelHost:     {"192.168.1.50"},
			labelUsername: {"admin"},
			labelPassword: {"secret"},
			"room":        {"front"},
		},
	})
	if err != nil {
		t.Fatalf("OnDeviceCreate error: %v", err)
	}
	if dev.ID == "" {
		t.Fatal("expected generated device ID")
	}
	if dev.Labels[labelPassword] != nil || dev.Labels[labelUsername] != nil {
		t.Fatalf("expected credential labels to be scrubbed: %#v", dev.Labels)
	}
	stored, err := raw.ReadRawDevice(dev.ID)
	if err != nil {
		t.Fatalf("expected raw credentials to be stored: %v", err)
	}
	var creds deviceCredentials
	if err := json.Unmarshal(stored, &creds); err != nil {
		t.Fatalf("invalid stored raw creds: %v", err)
	}
	if creds.Host != "192.168.1.50" || creds.Username != "admin" || creds.Password != "secret" {
		t.Fatalf("unexpected stored creds: %#v", creds)
	}
	if !creds.Caps.DoorbellPress || !creds.Caps.Motion || !creds.Caps.Snapshot || !creds.Caps.StreamMain {
		t.Fatalf("expected capabilities to be discovered, got %#v", creds.Caps)
	}
	if len(creds.ConfigOptions) == 0 {
		t.Fatalf("expected config options to be discovered, got none")
	}
}

func TestOnCommandSnapshotUsesStoredCredentials(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	sink := &fakeSink{}
	_, _ = p.OnInitialize(runner.Config{RawStore: raw, EventSink: sink}, types.Storage{})
	p.sink = sink

	creds := deviceCredentials{Host: "192.168.1.60", Username: "admin", Password: "pw", Scheme: "http", EventCodes: []string{"_DoorbellPress_"}}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("amcrest-192-168-1-60", data)

	p.clientFactory = func(c deviceCredentials) cameraClient {
		if c.Host != creds.Host || c.Username != creds.Username || c.Password != creds.Password {
			t.Fatalf("unexpected creds passed to client factory: %#v", c)
		}
		return &fakeClient{snapshot: []byte{1, 2, 3}}
	}

	_, err := p.OnCommand(types.Command{DeviceID: "amcrest-192-168-1-60"}, types.Entity{ID: snapshotButtonID})
	if err != nil {
		t.Fatalf("OnCommand error: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(sink.events))
	}
	if sink.events[0].EntityID != snapshotEntityID {
		t.Fatalf("expected snapshot entity event, got %s", sink.events[0].EntityID)
	}
}

func TestOnEntitiesListRespectsCapabilities(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	creds := deviceCredentials{
		Host:     "192.168.1.60",
		Username: "admin",
		Password: "pw",
		Scheme:   "http",
		ConfigOptions: []configOption{
			{Name: "Email", Enabled: true},
			{Name: "MotionDetect", Enabled: false},
		},
		Caps: deviceCapabilities{
			StreamMain: true,
			Snapshot:   true,
			Motion:     true,
			// DoorbellPress intentionally false.
		},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("amcrest-192-168-1-60", data)

	entities, err := p.OnEntitiesList("amcrest-192-168-1-60", nil)
	if err != nil {
		t.Fatalf("OnEntitiesList error: %v", err)
	}
	byID := map[string]types.Entity{}
	for _, e := range entities {
		byID[e.ID] = e
	}
	if _, ok := byID[doorbellEntityID]; ok {
		t.Fatalf("did not expect %s for non-doorbell capabilities", doorbellEntityID)
	}
	if _, ok := byID[motionEntityID]; !ok {
		t.Fatalf("expected %s entity", motionEntityID)
	}
	if _, ok := byID[snapshotEntityID]; !ok {
		t.Fatalf("expected %s entity", snapshotEntityID)
	}
	if _, ok := byID[streamEntityID]; !ok {
		t.Fatalf("expected %s entity", streamEntityID)
	}
	if cfg, ok := byID["cfg-email"]; !ok {
		t.Fatalf("expected cfg-email entity")
	} else {
		var payload map[string]any
		if err := json.Unmarshal(cfg.Data.Reported, &payload); err != nil {
			t.Fatalf("invalid cfg-email payload: %v", err)
		}
		if payload["enabled"] != true {
			t.Fatalf("expected cfg-email enabled=true, got %#v", payload)
		}
	}
	if cfg, ok := byID["cfg-motiondetect"]; !ok {
		t.Fatalf("expected cfg-motiondetect entity")
	} else {
		var payload map[string]any
		if err := json.Unmarshal(cfg.Data.Reported, &payload); err != nil {
			t.Fatalf("invalid cfg-motiondetect payload: %v", err)
		}
		if payload["enabled"] != false {
			t.Fatalf("expected cfg-motiondetect enabled=false, got %#v", payload)
		}
	}
}

func TestOnDeviceCreateFailsWhenConnectionFails(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{systemErr: errors.New("dial timeout")}
	}

	_, err := p.OnDeviceCreate(types.Device{Labels: map[string][]string{
		labelHost:     {"192.168.1.70"},
		labelUsername: {"admin"},
		labelPassword: {"pw"},
	}})
	if err == nil {
		t.Fatal("expected error when device connection fails")
	}
}
