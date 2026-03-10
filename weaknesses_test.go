package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slidebolt/plugin-amcrest/pkg/amcrest"
	runner "github.com/slidebolt/sdk-runner"
	"github.com/slidebolt/sdk-types"
)

// ============================================================================
// Weakness Test 1: Race Conditions in Concurrent Access
// ============================================================================

func TestConcurrentDeviceCreateDeleteRace(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			info:       map[string]string{"deviceType": "AD410", "serialNumber": "TEST"},
			version:    "1.0.0",
			eventCodes: []string{"_DoorbellPress_"},
		}
	}

	// Concurrent creates and deletes to expose race conditions
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			devID := fmt.Sprintf("device-%d", idx)
			dev, _ := p.OnDeviceCreate(types.Device{
				ID: devID,
				Labels: map[string][]string{
					labelHost:     {fmt.Sprintf("192.168.1.%d", idx)},
					labelUsername: {"admin"},
					labelPassword: {"pw"},
				},
			})
			if dev.ID != "" {
				p.OnDeviceDelete(dev.ID)
			}
		}(i)
	}
	wg.Wait()

	// Verify internal state consistency
	p.mu.Lock()
	loopCount := len(p.loopStops)
	profileCount := len(p.profiles)
	p.mu.Unlock()

	// After deletes, event loops should be cleaned up.
	// Profile cache entries may remain by design.
	if loopCount != 0 {
		t.Errorf("Race condition: expected 0 loops, got %d", loopCount)
	}
	if profileCount > 100 {
		t.Errorf("Profile cache unexpectedly grew beyond created devices: %d", profileCount)
	}
}

// ============================================================================
// Weakness Test 2: Nil Pointer Dereference Vulnerabilities
// ============================================================================

func TestOnCommandWithNilSink(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	// Intentionally not setting EventSink (nil)
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})
	p.sink = nil // Explicitly nil

	creds := deviceCredentials{
		Host:     "192.168.1.60",
		Username: "admin",
		Password: "pw",
		Scheme:   "http",
		Caps:     deviceCapabilities{Snapshot: true},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("device-1", data)

	// This should not panic even with nil sink
	_, err := p.OnCommand(types.Command{DeviceID: "device-1"}, types.Entity{ID: snapshotButtonID})
	// Should return an error but not crash
	if err == nil {
		t.Log("Expected error due to nil sink, but got none - this might be a bug")
	}
}

func TestEnsureEventLoopWithNilSink(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})
	p.sink = nil // Explicitly nil

	creds := deviceCredentials{
		Host:       "192.168.1.60",
		Username:   "admin",
		Password:   "pw",
		Scheme:     "http",
		EventCodes: []string{"_DoorbellPress_"},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("device-1", data)

	// This should exit early without creating a loop since sink is nil
	p.ensureEventLoop("device-1")

	p.mu.Lock()
	loopCount := len(p.loopStops)
	p.mu.Unlock()

	if loopCount != 0 {
		t.Errorf("Expected no loops with nil sink, got %d", loopCount)
	}
}

// ============================================================================
// Weakness Test 3: Error Handling and Silent Failures
// ============================================================================

func TestWriteDeviceCredsWithNilRawStore(t *testing.T) {
	p := NewPluginAdapter()
	// No RawStore set
	_, _ = p.OnInitialize(runner.Config{}, types.Storage{})

	creds := deviceCredentials{Host: "192.168.1.50", Username: "admin", Password: "pw"}
	err := p.writeDeviceCreds("device-1", creds)

	if err == nil {
		t.Fatal("Expected error when raw store is nil, but got nil")
	}
	if !strings.Contains(err.Error(), "raw store unavailable") {
		t.Fatalf("Expected 'raw store unavailable' error, got: %v", err)
	}
}

func TestOnDeviceCreateFailsSilentWhenStoreFails(t *testing.T) {
	p := NewPluginAdapter()
	failingStore := &failingRawStore{failWrite: true}
	_, _ = p.OnInitialize(runner.Config{RawStore: failingStore}, types.Storage{})

	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			info:    map[string]string{"deviceType": "AD410"},
			version: "1.0.0",
		}
	}

	_, err := p.OnDeviceCreate(types.Device{
		Labels: map[string][]string{
			labelHost:     {"192.168.1.50"},
			labelUsername: {"admin"},
			labelPassword: {"pw"},
		},
	})

	if err == nil {
		t.Fatal("Expected error when raw store write fails, but got nil")
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
func (m *failingRawStore) ReadRawEntity(deviceID, entityID string) (json.RawMessage, error) {
	return nil, errors.New("not implemented")
}
func (m *failingRawStore) WriteRawEntity(deviceID, entityID string, data json.RawMessage) error {
	return nil
}

// ============================================================================
// Weakness Test 4: Context Cancellation and Resource Leaks
// ============================================================================

func TestEventLoopDoesNotLeakOnContextCancellation(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	sink := &fakeSink{}
	_, _ = p.OnInitialize(runner.Config{RawStore: raw, EventSink: sink}, types.Storage{})

	subscribeCount := 0
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			eventCodes: []string{"_DoorbellPress_"},
		}
	}

	// Store custom client that blocks
	customClient := &blockingClient{blockUntil: make(chan struct{})}
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return customClient
	}

	creds := deviceCredentials{
		Host:       "192.168.1.60",
		Username:   "admin",
		Password:   "pw",
		Scheme:     "http",
		EventCodes: []string{"_DoorbellPress_"},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("device-1", data)

	// Start the event loop
	p.ensureEventLoop("device-1")

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Cancel the loop
	p.stopEventLoop("device-1")

	// Unblock the client
	close(customClient.blockUntil)

	// Wait for cleanup with timeout
	done := make(chan struct{})
	go func() {
		p.loopsWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success - loop cleaned up properly
	case <-time.After(2 * time.Second):
		t.Fatal("Event loop did not clean up properly - potential goroutine leak")
	}

	_ = subscribeCount
}

type blockingClient struct {
	blockUntil chan struct{}
}

func (b *blockingClient) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	select {
	case <-b.blockUntil:
		return map[string]string{"deviceType": "AD410"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *blockingClient) GetSoftwareVersion(ctx context.Context) (string, error) {
	select {
	case <-b.blockUntil:
		return "1.0.0", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (b *blockingClient) GetSnapshot(ctx context.Context) ([]byte, error) {
	select {
	case <-b.blockUntil:
		return []byte{1, 2, 3}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *blockingClient) GetEventCodes(ctx context.Context) ([]string, error) {
	select {
	case <-b.blockUntil:
		return []string{"_DoorbellPress_"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *blockingClient) GetAllConfig(ctx context.Context) (map[string]string, error) {
	select {
	case <-b.blockUntil:
		return map[string]string{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *blockingClient) SubscribeEvents(ctx context.Context, codes []string, onEvent func(amcrest.Event)) error {
	select {
	case <-b.blockUntil:
		return errors.New("connection closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ============================================================================
// Weakness Test 5: Timeout and Resource Exhaustion
// ============================================================================

func TestSlowClientCausesTimeoutIssues(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	// Client that takes longer than timeout
	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &slowClient{delay: 10 * time.Second}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	dev, err := p.OnDeviceCreate(types.Device{
		Labels: map[string][]string{
			labelHost:     {"192.168.1.50"},
			labelUsername: {"admin"},
			labelPassword: {"pw"},
		},
	})
	elapsed := time.Since(start)

	// Should fail quickly due to context timeout, not wait 10 seconds
	if err == nil {
		t.Fatal("Expected timeout error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Timeout not respected: took %v, expected < 5s", elapsed)
	}

	_ = dev
	_ = ctx
}

type slowClient struct {
	delay time.Duration
}

func (s *slowClient) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	time.Sleep(s.delay)
	return nil, ctx.Err()
}
func (s *slowClient) GetSoftwareVersion(ctx context.Context) (string, error) {
	time.Sleep(s.delay)
	return "", ctx.Err()
}
func (s *slowClient) GetSnapshot(ctx context.Context) ([]byte, error) {
	time.Sleep(s.delay)
	return nil, ctx.Err()
}
func (s *slowClient) GetEventCodes(ctx context.Context) ([]string, error) {
	time.Sleep(s.delay)
	return nil, ctx.Err()
}
func (s *slowClient) GetAllConfig(ctx context.Context) (map[string]string, error) {
	time.Sleep(s.delay)
	return nil, ctx.Err()
}
func (s *slowClient) SubscribeEvents(ctx context.Context, codes []string, onEvent func(amcrest.Event)) error {
	<-ctx.Done()
	return ctx.Err()
}

// ============================================================================
// Weakness Test 6: Data Validation Issues
// ============================================================================

func TestInvalidDeviceIDCharacters(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &fakeClient{
			info: map[string]string{"deviceType": "AD410"},
		}
	}

	// Test with various invalid characters
	invalidHosts := []string{
		"192.168.1.50:8080", // Port number
		"host/with/path",    // Path separator
		"host?name=test",    // Query string (would break URLs)
		"",                  // Empty host
		"   ",               // Whitespace only
	}

	for _, host := range invalidHosts {
		dev, err := p.OnDeviceCreate(types.Device{
			Labels: map[string][]string{
				labelHost:     {host},
				labelUsername: {"admin"},
				labelPassword: {"pw"},
			},
		})

		if err == nil && host == "" {
			t.Errorf("Should fail with empty host, but got device: %+v", dev)
		}

		if dev.ID != "" {
			// Check if ID contains problematic characters
			if strings.Contains(dev.ID, ":") || strings.Contains(dev.ID, "/") {
				t.Errorf("Device ID contains invalid characters: %s", dev.ID)
			}
		}
	}
}

func TestMalformedStoredCredentials(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	// Write invalid JSON as credentials
	_ = raw.WriteRawDevice("device-1", json.RawMessage(`{invalid json`))

	// Try to read credentials - should handle gracefully
	creds, ok := p.readOrCachedCreds("device-1")
	if ok {
		t.Errorf("Should not parse invalid JSON, but got: %+v", creds)
	}

	// Write valid JSON but with missing required fields
	_ = raw.WriteRawDevice("device-2", json.RawMessage(`{"host":"","username":"","password":""}`))
	creds, ok = p.readOrCachedCreds("device-2")
	if ok {
		t.Errorf("Should not accept empty credentials, but got: %+v", creds)
	}

	// Write credentials with invalid scheme
	_ = raw.WriteRawDevice("device-3", json.RawMessage(`{"host":"192.168.1.50","username":"admin","password":"pw","scheme":"ftp"}`))
	creds, ok = p.readOrCachedCreds("device-3")
	if ok && creds.Scheme == "ftp" {
		t.Errorf("Should reject invalid scheme 'ftp', but got: %+v", creds)
	}
}

// ============================================================================
// Weakness Test 7: Event Handling Edge Cases
// ============================================================================

func TestHandleEventWithUnknownCodes(t *testing.T) {
	p := NewPluginAdapter()
	sink := &fakeSink{}
	p.sink = sink

	// Unknown event codes should be silently ignored
	p.handleEvent("device-1", amcrest.Event{Code: "UnknownCode", Action: "start"})
	p.handleEvent("device-1", amcrest.Event{Code: "", Action: "start"})
	p.handleEvent("device-1", amcrest.Event{Code: "VideoMotion", Action: ""})
	p.handleEvent("device-1", amcrest.Event{Code: "VideoMotion", Action: "invalid"})

	// Only known codes should be emitted; unknown codes are ignored.
	if len(sink.events) != 2 {
		t.Errorf("Expected 2 motion events, got %d", len(sink.events))
	}

	// Check specific events
	foundMotion := false
	for _, e := range sink.events {
		if e.EntityID == motionEntityID {
			foundMotion = true
			var payload map[string]interface{}
			json.Unmarshal(e.Payload, &payload)
			if payload["state"] != "on" && payload["state"] != "off" {
				t.Errorf("Unexpected motion state: %v", payload["state"])
			}
		}
	}

	if !foundMotion {
		t.Error("Expected at least one motion event")
	}
}

func TestEmitEventWithLargePayload(t *testing.T) {
	p := NewPluginAdapter()
	sink := &fakeSink{}
	p.sink = sink

	// Test with very large state
	largeState := strings.Repeat("a", 1000000)
	p.emitBinaryState("device-1", motionEntityID, largeState, "motion_detected")

	if len(sink.events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(sink.events))
	}

	// Verify the large payload was accepted (potential DoS vector)
	if len(sink.events[0].Payload) < 1000000 {
		t.Error("Large payload was truncated unexpectedly")
	}
}

// ============================================================================
// Weakness Test 8: Configuration Discovery Edge Cases
// ============================================================================

func TestDiscoverCapabilitiesWithPartialFailures(t *testing.T) {
	p := NewPluginAdapter()

	// Client that fails only snapshot but succeeds on event codes
	client := &fakeClient{
		snapshotErr: errors.New("snapshot unavailable"),
		eventCodes:  []string{"_DoorbellPress_", "VideoMotion"},
	}

	creds := deviceCredentials{Model: "AD410"}
	caps := p.discoverCapabilities(client, creds)

	// Should still discover other capabilities even if snapshot fails
	if caps.Snapshot {
		t.Error("Expected Snapshot to be false when it fails")
	}
	if !caps.Motion {
		t.Error("Expected Motion to be true from event codes")
	}
	if !caps.StreamMain {
		t.Error("Expected StreamMain to always be true")
	}
	// Doorbell press should be true due to AD410 heuristic
	if !caps.DoorbellPress {
		t.Error("Expected DoorbellPress to be true for AD410 model")
	}
}

func TestDiscoverConfigWithMalformedData(t *testing.T) {
	p := NewPluginAdapter()

	client := &fakeClient{
		allConfig: map[string]string{
			"invalid":                          "no prefix",
			"table.All.":                       "empty section",
			"table.All.ValidSection.Enable":    "not-a-boolean",
			"table.All.MotionDetect[0].Enable": "true",
			"table.All.MotionDetect[1].Enable": "false",
			"table.All.Email.Enable":           "true",
			"table.All..Enable":                "true",  // Double dot
			"table.All.[].Enable":              "true",  // Empty brackets
			"table.All.Normal":                 "value", // No Enable suffix
		},
	}

	options := p.discoverConfigOptions(client)

	// Should handle malformed data gracefully
	foundSections := make(map[string]bool)
	for _, opt := range options {
		foundSections[opt.Name] = opt.Enabled
	}

	// Valid sections should be found
	if !foundSections["MotionDetect"] {
		t.Error("Expected MotionDetect to be found")
	}
	if !foundSections["Email"] {
		t.Error("Expected Email to be found")
	}
	if foundSections["ValidSection"] {
		t.Error("ValidSection should not be found (value is 'not-a-boolean')")
	}
}

// ============================================================================
// Weakness Test 9: Sanitization and Security
// ============================================================================

func TestSanitizeEntityIDWithInjectionAttempts(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"../../../etc/passwd", "etc-passwd"},
		{"config&attack", "config-attack"},
		{"config<script>", "config-script"},
		{"CONFIG NAME", "config-name"},
		{"config:name", "config-name"},
		{"", "unknown"},
		{"   ", "unknown"},
		{"!@#$%", "unknown"},
	}

	for _, tc := range testCases {
		result := sanitizeEntityID(tc.input)
		if result != tc.expected {
			t.Errorf("sanitizeEntityID(%q) = %q, expected %q", tc.input, result, tc.expected)
		}
	}
}

func TestScrubSensitiveLabelsPartial(t *testing.T) {
	labels := map[string][]string{
		labelHost:     {"192.168.1.50"},
		labelUsername: {"admin"},
		labelPassword: {"secret"},
		labelScheme:   {"https"},
		labelEvents:   {"_DoorbellPress_"},
		"room":        {"front"},
	}

	scrubbed := scrubSensitiveLabels(labels)

	// Sensitive labels should be removed
	sensitiveKeys := []string{labelHost, labelUsername, labelPassword, labelScheme, labelEvents}
	for _, key := range sensitiveKeys {
		if _, exists := scrubbed[key]; exists {
			t.Errorf("Sensitive label %q should be scrubbed", key)
		}
	}

	// Non-sensitive should remain
	if room, exists := scrubbed["room"]; !exists || room[0] != "front" {
		t.Error("Non-sensitive label 'room' should be preserved")
	}
}

// ============================================================================
// Weakness Test 10: Memory and Performance Issues
// ============================================================================

func TestEventLoopRapidRestart(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	sink := &fakeSink{}
	_, _ = p.OnInitialize(runner.Config{RawStore: raw, EventSink: sink}, types.Storage{})

	subscribeCount := 0
	var mu sync.Mutex

	p.clientFactory = func(creds deviceCredentials) cameraClient {
		return &rapidFailClient{
			count: &subscribeCount,
			mu:    &mu,
		}
	}

	creds := deviceCredentials{
		Host:       "192.168.1.60",
		Username:   "admin",
		Password:   "pw",
		Scheme:     "http",
		EventCodes: []string{"_DoorbellPress_"},
	}
	data, _ := json.Marshal(creds)
	_ = raw.WriteRawDevice("device-1", data)

	// Start event loop
	p.ensureEventLoop("device-1")

	// Let it run briefly to accumulate restarts
	time.Sleep(2 * time.Second)

	// Stop the loop
	p.stopEventLoop("device-1")
	p.loopsWG.Wait()

	mu.Lock()
	count := subscribeCount
	mu.Unlock()

	// With 1-second backoff, should have ~2 attempts
	if count == 0 {
		t.Error("Event loop never attempted to subscribe")
	}
	if count > 10 {
		t.Errorf("Too many subscription attempts (%d) - potential busy loop", count)
	}
}

type rapidFailClient struct {
	count *int
	mu    *sync.Mutex
}

func (r *rapidFailClient) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	return map[string]string{"deviceType": "AD410"}, nil
}
func (r *rapidFailClient) GetSoftwareVersion(ctx context.Context) (string, error) {
	return "1.0.0", nil
}
func (r *rapidFailClient) GetSnapshot(ctx context.Context) ([]byte, error) {
	return []byte{1, 2, 3}, nil
}
func (r *rapidFailClient) GetEventCodes(ctx context.Context) ([]string, error) {
	return []string{"_DoorbellPress_"}, nil
}
func (r *rapidFailClient) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *rapidFailClient) SubscribeEvents(ctx context.Context, codes []string, onEvent func(amcrest.Event)) error {
	r.mu.Lock()
	*r.count++
	r.mu.Unlock()
	return errors.New("connection refused")
}

func TestReadOrCachedCredsWithManyDevices(t *testing.T) {
	p := NewPluginAdapter()
	raw := newMockRawStore()
	_, _ = p.OnInitialize(runner.Config{RawStore: raw}, types.Storage{})

	// Create many devices
	for i := 0; i < 1000; i++ {
		creds := deviceCredentials{
			Host:     fmt.Sprintf("192.168.%d.%d", i/256, i%256),
			Username: "admin",
			Password: "pw",
			Scheme:   "http",
			Caps:     deviceCapabilities{Snapshot: true, Motion: true},
		}
		data, _ := json.Marshal(creds)
		_ = raw.WriteRawDevice(fmt.Sprintf("device-%d", i), data)
	}

	// Read all devices concurrently
	var wg sync.WaitGroup
	errors := make(chan error, 1000)

	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("device-%d", idx)
			_, ok := p.readOrCachedCreds(deviceID)
			if !ok {
				errors <- fmt.Errorf("failed to read device %s", deviceID)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	errorCount := 0
	for err := range errors {
		t.Logf("Error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Errorf("Had %d errors reading credentials", errorCount)
	}
}
