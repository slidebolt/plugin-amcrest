package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/slidebolt/plugin-amcrest/pkg/amcrest"
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
	mu      sync.RWMutex
}

func newMockRawStore() *mockRawStore { return &mockRawStore{devices: map[string]json.RawMessage{}} }

func (m *mockRawStore) ReadRawDevice(deviceID string) (json.RawMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.devices[deviceID]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (m *mockRawStore) WriteRawDevice(deviceID string, data json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[deviceID] = append([]byte(nil), data...)
	return nil
}

func TestResolveAndHydrateDevice_ScrubsLabelsAndStoresCreds(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	initTestAdapter(p, raw, nil)
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			info:       map[string]string{"deviceType": "AD410", "serialNumber": "SER123"},
			version:    "1.0.0",
			snapshot:   []byte{1, 2},
			eventCodes: []string{"_DoorbellPress_", "VideoMotion"},
		}
	}

	dev := types.Device{
		ID: "amcrest-1",
		Labels: map[string][]string{
			labelHost:     {"192.168.1.50"},
			labelUsername: {"admin"},
			labelPassword: {"secret"},
			"room":        {"front"},
		},
	}
	updated, creds, ok := p.resolveAndHydrateDevice(dev)
	if !ok {
		t.Fatal("resolveAndHydrateDevice returned ok=false")
	}
	if creds.Host != "192.168.1.50" {
		t.Fatalf("unexpected creds host: %s", creds.Host)
	}
	if updated.Labels[labelPassword] != nil || updated.Labels[labelUsername] != nil {
		t.Fatalf("expected credential labels to be scrubbed: %#v", updated.Labels)
	}
	if _, err := raw.ReadRawDevice(updated.ID); err != nil {
		t.Fatalf("expected stored creds for %s: %v", updated.ID, err)
	}
}

func TestBuildEntitiesForDevice_RespectsCapabilities(t *testing.T) {
	creds := deviceCredentials{
		Caps: deviceCapabilities{
			StreamMain:    true,
			Snapshot:      true,
			Motion:        true,
			DoorbellPress: false,
		},
	}
	entities := buildEntitiesForDevice("amcrest-1", creds)
	byID := map[string]types.Entity{}
	for _, e := range entities {
		byID[e.ID] = e
	}
	for _, id := range []string{availabilityEntity, streamEntityID, snapshotEntityID, snapshotButtonID, motionEntityID} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("expected entity %q", id)
		}
	}
	if _, ok := byID[doorbellEntityID]; ok {
		t.Fatalf("did not expect %q for this capability set", doorbellEntityID)
	}
}

func TestRunCommandSnapshotUsesStoredCredentials(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	sink := &fakeSink{}
	initTestAdapter(p, raw, sink)
	p.pctx.Events = testEventService{sink: sink}

	creds := deviceCredentials{
		Host: "192.168.1.60", Username: "admin", Password: "pw", Scheme: "http",
		Caps: deviceCapabilities{Snapshot: true},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("amcrest-192-168-1-60", data)

	p.clientFactory = func(c deviceCredentials) cameraClient {
		if c.Host != creds.Host || c.Username != creds.Username || c.Password != creds.Password {
			t.Fatalf("unexpected creds passed to client factory: %#v", c)
		}
		return &fakeClient{snapshot: []byte{1, 2, 3}}
	}

	_, err := p.runCommand(types.Command{DeviceID: "amcrest-192-168-1-60"}, types.Entity{ID: snapshotButtonID})
	if err != nil {
		t.Fatalf("runCommand error: %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].EntityID != snapshotEntityID {
		t.Fatalf("expected one %q event, got %#v", snapshotEntityID, sink.events)
	}
}
