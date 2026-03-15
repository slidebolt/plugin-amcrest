package main

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/slidebolt/sdk-types"
)

func TestWriteDeviceCredsWithNoStorage(t *testing.T) {
	p := NewPluginAdapter()
	initTestAdapter(p, nil, nil)

	err := p.writeDeviceCreds("device-1", deviceCredentials{Host: "x", Username: "u", Password: "p", Scheme: "http"})
	if err == nil {
		t.Fatal("expected error when no rawStore/registry is available")
	}
	if err.Error() != "raw store unavailable" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureEventLoopWithNilEvents(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	initTestAdapter(p, raw, nil)

	creds := deviceCredentials{
		Host: "192.168.1.60", Username: "admin", Password: "pw", Scheme: "http",
		EventCodes: []string{"_DoorbellPress_"},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("device-1", data)

	p.pctx.Events = nil
	p.ensureEventLoop("device-1")

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.loopStops) != 0 {
		t.Fatalf("expected no active loops, got %d", len(p.loopStops))
	}
}

func TestConcurrentStoreCreds(t *testing.T) {
	p := NewPluginAdapter()
	initTestAdapter(p, nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := "dev-" + string(rune('a'+(idx%26)))
			p.storeCreds(id, deviceCredentials{
				Host: "h", Username: "u", Password: "p", Scheme: "http",
			})
		}(i)
	}
	wg.Wait()

	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.profiles) == 0 {
		t.Fatal("expected profile cache entries after concurrent writes")
	}
}

type failingRawStore struct {
	failRead  bool
	failWrite bool
}

func (m *failingRawStore) ReadRawDevice(deviceID string) (json.RawMessage, error) {
	if m.failRead {
		return nil, errors.New("read failed")
	}
	return nil, errors.New("not found")
}
func (m *failingRawStore) WriteRawDevice(deviceID string, data json.RawMessage) error {
	if m.failWrite {
		return errors.New("write failed")
	}
	return nil
}

func TestWriteDeviceCredsRawStoreErrorPropagates(t *testing.T) {
	p := NewPluginAdapter()
	failing := &failingRawStore{failWrite: true}
	initTestAdapter(p, failing, nil)

	err := p.writeDeviceCreds("device-1", deviceCredentials{Host: "h", Username: "u", Password: "p", Scheme: "http"})
	if err == nil {
		t.Fatal("expected raw store write failure")
	}
}

func TestOnCommandUnsupportedEntityNoError(t *testing.T) {
	p := NewPluginAdapter()
	initTestAdapter(p, nil, nil)

	err := p.OnCommand(types.Command{DeviceID: "x"}, types.Entity{ID: "not_snapshot"})
	if err != nil {
		t.Fatalf("unexpected error for unsupported entity pass-through: %v", err)
	}
}
