// plugin-amcrest integrates Amcrest event-capable devices with SlideBolt.
//
// Features:
//   - HTTP Digest authentication
//   - Manual per-device provisioning
//   - Device info and event code querying
//   - Long-poll event stream ingestion
//   - Generic event-derived entity updates
package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	contract "github.com/slidebolt/sb-contract"
	domain "github.com/slidebolt/sb-domain"
	storage "github.com/slidebolt/sb-storage-sdk"
)

const PluginID = "plugin-amcrest"

type CameraConfig struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
	Scheme   string `json:"scheme,omitempty"`
	Timeout  int    `json:"timeout_ms,omitempty"`
}

type PluginConfig struct {
	Cameras []CameraConfig `json:"cameras"`
}

type AmcrestClient struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
	// RawLineHook, if set, is invoked for every non-empty line read from
	// the event stream — including multipart boundaries and headers — for
	// debugging. Never invoked in production code paths that don't set it.
	RawLineHook func(line string)
}

func NewAmcrestClient(baseURL, username, password string, timeout time.Duration) *AmcrestClient {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &AmcrestClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Username:   username,
		Password:   password,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

func (c *AmcrestClient) GetSystemInfo(ctx context.Context) (map[string]string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/magicBox.cgi?action=getSystemInfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseKV(resp.Body), nil
}

func (c *AmcrestClient) GetSoftwareVersion(ctx context.Context) (string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/magicBox.cgi?action=getSoftwareVersion")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	kv := parseKV(resp.Body)
	return kv["version"], nil
}

func (c *AmcrestClient) GetSnapshot(ctx context.Context) ([]byte, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/snapshot.cgi")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *AmcrestClient) GetEventCodes(ctx context.Context) ([]string, error) {
	resp, err := c.getWithDigest(ctx, "/cgi-bin/eventManager.cgi?action=getEventIndexes&code=All")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseEventCodes(resp.Body), nil
}

// Event is a single Amcrest event decoded from the long-poll event stream.
// Raw preserves the original "Code=...;action=...;index=...;data=..." line.
type Event struct {
	Code   string
	Action string
	Index  string
	Data   string
	Raw    string
}

// StreamEvents opens a long-poll HTTP Digest connection to eventManager.cgi
// and invokes fn for every event received until ctx is cancelled or the
// stream errors. codes is a bracketed filter like "[All]" or
// "[VideoMotion,CallNoAnswered]". Returns the context/stream error, or nil
// if ctx was cancelled cleanly.
func (c *AmcrestClient) StreamEvents(ctx context.Context, codes string, fn func(Event)) error {
	if codes == "" {
		codes = "[All]"
	}
	// heartbeat=N asks the camera to emit a keepalive line every N seconds
	// so the long poll proves liveness even when no real events fire.
	path := "/cgi-bin/eventManager.cgi?action=attach&codes=" + codes + "&heartbeat=5"

	// Long-poll needs a client with no overall timeout; reuse transport only.
	streamClient := &http.Client{Transport: http.DefaultTransport}

	doReq := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
		if err != nil {
			return nil, err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return streamClient.Do(req)
	}

	resp, err := doReq("")
	if err != nil {
		return fmt.Errorf("event stream initial request: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if !strings.HasPrefix(challenge, "Digest") {
			return fmt.Errorf("expected digest challenge, got: %s", challenge)
		}
		params := parseDigestChallenge(challenge)
		auth := buildDigestAuthHeader(c.Username, c.Password, http.MethodGet, path, params)
		resp, err = doReq(auth)
		if err != nil {
			return fmt.Errorf("event stream authed request: %w", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream: unexpected status %d", resp.StatusCode)
	}
	if c.RawLineHook != nil {
		c.RawLineHook(fmt.Sprintf("[debug] status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type")))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if c.RawLineHook != nil {
			c.RawLineHook(line)
		}
		if line == "Heartbeat" {
			fn(Event{Code: "Heartbeat", Raw: line})
			continue
		}
		if !strings.HasPrefix(line, "Code=") {
			continue
		}
		fn(parseEventLine(line))
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("event stream read: %w", err)
	}
	return nil
}

func parseEventLine(line string) Event {
	ev := Event{Raw: line}
	for _, part := range strings.Split(line, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Code":
			ev.Code = strings.TrimSpace(v)
		case "action":
			ev.Action = strings.TrimSpace(v)
		case "index":
			ev.Index = strings.TrimSpace(v)
		case "data":
			ev.Data = strings.TrimSpace(v)
		}
	}
	return ev
}

func (c *AmcrestClient) getWithDigest(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()

	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Digest") {
		return nil, fmt.Errorf("expected digest challenge, got: %s", challenge)
	}
	params := parseDigestChallenge(challenge)
	auth := buildDigestAuthHeader(c.Username, c.Password, req.Method, req.URL.RequestURI(), params)

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", auth)
	return c.HTTPClient.Do(req2)
}

func parseDigestChallenge(header string) map[string]string {
	header = strings.TrimPrefix(header, "Digest ")
	parts := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(part[:idx])
		val := strings.Trim(strings.TrimSpace(part[idx+1:]), `"`)
		parts[key] = val
	}
	return parts
}

func buildDigestAuthHeader(username, password, method, uri string, params map[string]string) string {
	realm := params["realm"]
	nonce := params["nonce"]
	opaque := params["opaque"]
	qop := params["qop"]

	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)

	if strings.Contains(qop, "auth") {
		nc := "00000001"
		cnonce := md5hex(fmt.Sprintf("%d", time.Now().UnixNano()))[:8]
		resp := md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s", opaque="%s"`,
			username, realm, nonce, uri, nc, cnonce, resp, opaque)
	}

	resp := md5hex(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", opaque="%s"`,
		username, realm, nonce, uri, resp, opaque)
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

func parseKV(r io.Reader) map[string]string {
	out := map[string]string{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return out
}

func parseEventCodes(r io.Reader) []string {
	s := bufio.NewScanner(r)
	out := make([]string, 0, 16)
	seen := map[string]struct{}{}
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if !strings.HasPrefix(key, "events[") {
			continue
		}
		value := strings.TrimSpace(line[idx+1:])
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// DeviceStatus is plugin-owned runtime state for an Amcrest device.
type DeviceStatus struct {
	Connected       bool
	LastError       string
	Version         string
	DeviceInfo      map[string]string
	EventCodes      []string
	LastHeartbeatAt string
	LastEventAt     string
	LastEventCode   string
	LastEventAction string
	LastEventRaw    string
	Reconnects      uint64
	ActiveEvents    map[string]bool
	EventCounts     map[string]int
}

type App struct {
	store storage.Storage

	mu      sync.RWMutex
	clients map[string]*AmcrestClient
	devices map[string]domain.Device
	status  map[string]DeviceStatus
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New() *App {
	return &App{status: make(map[string]DeviceStatus)}
}

// Status returns the current runtime status for a device.
func (a *App) Status(cameraID string) (DeviceStatus, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.status[cameraID]
	return s, ok
}

func (a *App) setStatus(cameraID string, update func(*DeviceStatus)) DeviceStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.status[cameraID]
	if s.ActiveEvents == nil {
		s.ActiveEvents = map[string]bool{}
	}
	if s.EventCounts == nil {
		s.EventCounts = map[string]int{}
	}
	update(&s)
	a.status[cameraID] = s
	return cloneStatus(s)
}

func (a *App) Hello() contract.HelloResponse {
	return contract.HelloResponse{
		ID:              PluginID,
		Kind:            contract.KindPlugin,
		ContractVersion: contract.ContractVersion,
		DependsOn:       []string{"messenger", "storage"},
	}
}

func (a *App) OnStart(deps map[string]json.RawMessage) (json.RawMessage, error) {
	store, err := storage.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect storage: %w", err)
	}
	a.store = store

	a.clients = make(map[string]*AmcrestClient)
	a.devices = make(map[string]domain.Device)
	if a.status == nil {
		a.status = make(map[string]DeviceStatus)
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())

	if err := a.reconcileStoredDevices(); err != nil {
		log.Printf("plugin-amcrest: device discovery error: %v", err)
	}

	log.Println("plugin-amcrest: started")
	return nil, nil
}

func (a *App) OnShutdown() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	if a.store != nil {
		a.store.Close()
	}
	return nil
}

func (a *App) reconcileStoredDevices() error {
	entries, err := a.store.Search(PluginID + ".*")
	if err != nil {
		return fmt.Errorf("search devices: %w", err)
	}

	ctx := context.Background()
	for _, entry := range entries {
		if strings.Count(entry.Key, ".") != 1 {
			continue
		}

		var device domain.Device
		if err := json.Unmarshal(entry.Data, &device); err != nil {
			log.Printf("plugin-amcrest: invalid stored device %s: %v", entry.Key, err)
			continue
		}
		a.mu.Lock()
		a.devices[device.ID] = device
		a.mu.Unlock()

		cfg, err := cameraConfigFromPrivate(a.store, device)
		if err != nil {
			log.Printf("plugin-amcrest: cannot load credentials for %s: %v", device.ID, err)
			continue
		}

		client := newClientFromConfig(cfg)
		a.clients[cfg.ID] = client

		status := DeviceStatus{}
		if info, err := client.GetSystemInfo(ctx); err != nil {
			status.LastError = err.Error()
			log.Printf("plugin-amcrest: failed to connect to device %s: %v", cfg.ID, err)
		} else {
			status.Connected = true
			status.DeviceInfo = info
		}
		if version, err := client.GetSoftwareVersion(ctx); err == nil {
			status.Version = version
		}
		if codes, err := client.GetEventCodes(ctx); err == nil {
			status.EventCodes = codes
		}
		current := a.setStatus(cfg.ID, func(s *DeviceStatus) { *s = status })
		if err := a.syncDeviceEntities(device, current); err != nil {
			log.Printf("plugin-amcrest: failed to save device entities for %s: %v", cfg.ID, err)
		}
		log.Printf("plugin-amcrest: synced device %s (connected=%t)", cfg.ID, current.Connected)

		a.wg.Add(1)
		go func(device domain.Device, client *AmcrestClient) {
			defer a.wg.Done()
			a.runEventLoop(a.ctx, device, client)
		}(device, client)
	}
	return nil
}

func newClientFromConfig(cfg CameraConfig) *AmcrestClient {
	port := cfg.Port
	if port == 0 {
		port = 80
	}
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "http"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10000
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, cfg.Host, port)
	return NewAmcrestClient(baseURL, cfg.Username, cfg.Password, time.Duration(timeout)*time.Millisecond)
}

// cameraConfigFromPrivate loads camera credentials from the device's
// private sidecar. Credentials are stored as a JSON object matching
// CameraConfig — the whole struct round-trips through the private blob.
func cameraConfigFromPrivate(store storage.Storage, device domain.Device) (CameraConfig, error) {
	cfg := CameraConfig{ID: device.ID}
	if cfg.ID == "" {
		return cfg, fmt.Errorf("missing device id")
	}
	raw, err := store.GetPrivate(device)
	if err != nil {
		return cfg, fmt.Errorf("get private: %w", err)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, fmt.Errorf("no private credentials stored")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("unmarshal private: %w", err)
	}
	cfg.ID = device.ID
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return cfg, fmt.Errorf("private must include host, username, password")
	}
	return cfg, nil
}

func (a *App) runEventLoop(ctx context.Context, device domain.Device, client *AmcrestClient) {
	deviceID := device.ID
	backoff := time.Second
	connectedOnce := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		status := a.setStatus(deviceID, func(s *DeviceStatus) {
			if connectedOnce {
				s.Reconnects++
			}
			s.Connected = true
			s.LastError = ""
		})
		if err := a.syncDeviceEntities(device, status); err != nil {
			log.Printf("plugin-amcrest: sync before stream %s: %v", deviceID, err)
		}

		err := client.StreamEvents(ctx, "[All]", func(ev Event) {
			status := a.applyEvent(deviceID, ev)
			if err := a.syncDeviceEntities(device, status); err != nil {
				log.Printf("plugin-amcrest: sync event entities %s: %v", deviceID, err)
			}
		})
		if ctx.Err() != nil {
			return
		}
		connectedOnce = true

		status = a.setStatus(deviceID, func(s *DeviceStatus) {
			s.Connected = false
			if err != nil {
				s.LastError = err.Error()
			}
		})
		if syncErr := a.syncDeviceEntities(device, status); syncErr != nil {
			log.Printf("plugin-amcrest: sync disconnect entities %s: %v", deviceID, syncErr)
		}
		if err != nil {
			log.Printf("plugin-amcrest: event stream error for %s: %v", deviceID, err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *App) applyEvent(deviceID string, ev Event) DeviceStatus {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return a.setStatus(deviceID, func(s *DeviceStatus) {
		s.Connected = true
		s.LastError = ""
		if ev.Code == "Heartbeat" {
			s.LastHeartbeatAt = now
			return
		}
		if s.ActiveEvents == nil {
			s.ActiveEvents = map[string]bool{}
		}
		if s.EventCounts == nil {
			s.EventCounts = map[string]int{}
		}
		s.EventCounts[ev.Code]++
		if isStartAction(ev.Action) {
			s.ActiveEvents[ev.Code] = true
		}
		if isStopAction(ev.Action) {
			s.ActiveEvents[ev.Code] = false
		}
		if _, ok := s.ActiveEvents[ev.Code]; !ok {
			s.ActiveEvents[ev.Code] = false
		}
		s.LastHeartbeatAt = now
		s.LastEventAt = now
		s.LastEventCode = ev.Code
		s.LastEventAction = ev.Action
		s.LastEventRaw = ev.Raw
	})
}

func (a *App) syncDeviceEntities(device domain.Device, status DeviceStatus) error {
	desired := make(map[string]domain.Entity)
	for _, entity := range a.desiredEntities(device, status) {
		desired[entity.Key()] = entity
	}
	existing, err := a.store.Search(PluginID + "." + device.ID + ".>")
	if err != nil {
		return fmt.Errorf("search device entities: %w", err)
	}
	for _, key := range sortedEntityKeys(desired) {
		if err := saveEntityIfChanged(a.store, desired[key]); err != nil {
			return err
		}
	}
	for _, entry := range existing {
		if _, ok := desired[entry.Key]; ok {
			continue
		}
		if err := a.store.Delete(entityKey(entry.Key)); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) desiredEntities(device domain.Device, status DeviceStatus) []domain.Entity {
	deviceID := device.ID
	name := device.Name
	if name == "" {
		name = deviceID
	}
	entities := []domain.Entity{
		{
			ID:       "connection",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "diagnostic",
			Name:     "Amcrest Event Connection",
			State: map[string]any{
				"connected":         status.Connected,
				"lastError":         status.LastError,
				"version":           status.Version,
				"deviceInfo":        status.DeviceInfo,
				"eventCodes":        status.EventCodes,
				"lastHeartbeatAt":   status.LastHeartbeatAt,
				"lastEventAt":       status.LastEventAt,
				"lastEventCode":     status.LastEventCode,
				"lastEventAction":   status.LastEventAction,
				"reconnects":        status.Reconnects,
				"deviceDisplayName": name,
			},
		},
		{
			ID:       "supported-event-codes",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "sensor",
			Name:     "Supported Event Codes",
			State:    domain.Sensor{Value: append([]string(nil), status.EventCodes...)},
		},
		{
			ID:       "last-event-code",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "sensor",
			Name:     "Last Event Code",
			State:    domain.Sensor{Value: status.LastEventCode},
		},
		{
			ID:       "last-event-action",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "sensor",
			Name:     "Last Event Action",
			State:    domain.Sensor{Value: status.LastEventAction},
		},
		{
			ID:       "last-event-at",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "sensor",
			Name:     "Last Event At",
			State:    domain.Sensor{Value: status.LastEventAt},
		},
		{
			ID:       "last-event-raw",
			Plugin:   PluginID,
			DeviceID: deviceID,
			Type:     "sensor",
			Name:     "Last Event Raw",
			State:    domain.Sensor{Value: status.LastEventRaw},
		},
	}

	for _, code := range status.EventCodes {
		safe := sanitizeID(code)
		display := displayEventCode(code)
		entities = append(entities,
			domain.Entity{
				ID:       "event-" + safe,
				Plugin:   PluginID,
				DeviceID: deviceID,
				Type:     "binary_sensor",
				Name:     display,
				State:    domain.BinarySensor{On: status.ActiveEvents[code]},
			},
			domain.Entity{
				ID:       "event-" + safe + "-count",
				Plugin:   PluginID,
				DeviceID: deviceID,
				Type:     "sensor",
				Name:     display + " Count",
				State:    domain.Sensor{Value: status.EventCounts[code]},
			},
		)
	}
	return entities
}

func cloneStatus(in DeviceStatus) DeviceStatus {
	out := in
	if in.DeviceInfo != nil {
		out.DeviceInfo = make(map[string]string, len(in.DeviceInfo))
		for k, v := range in.DeviceInfo {
			out.DeviceInfo[k] = v
		}
	}
	if in.EventCodes != nil {
		out.EventCodes = append([]string(nil), in.EventCodes...)
	}
	if in.ActiveEvents != nil {
		out.ActiveEvents = make(map[string]bool, len(in.ActiveEvents))
		for k, v := range in.ActiveEvents {
			out.ActiveEvents[k] = v
		}
	}
	if in.EventCounts != nil {
		out.EventCounts = make(map[string]int, len(in.EventCounts))
		for k, v := range in.EventCounts {
			out.EventCounts[k] = v
		}
	}
	return out
}

func saveEntityIfChanged(store storage.Storage, entity domain.Entity) error {
	body, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	current, err := store.Get(entity)
	if err == nil && normalizedJSON(current) == normalizedJSON(body) {
		return nil
	}
	return store.Save(entity)
}

func normalizedJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return string(data)
	}
	return buf.String()
}

func sortedEntityKeys(m map[string]domain.Entity) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type entityKey string

func (k entityKey) Key() string { return string(k) }

func sanitizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func displayEventCode(code string) string {
	code = strings.Trim(code, "_")
	code = strings.ReplaceAll(code, "_", " ")
	code = strings.ReplaceAll(code, "-", " ")
	parts := strings.Fields(code)
	for i, part := range parts {
		if part == strings.ToUpper(part) {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
	}
	return strings.Join(parts, " ")
}

func isStartAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start", "pulse", "on":
		return true
	default:
		return false
	}
}

func isStopAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "stop", "end", "off":
		return true
	default:
		return false
	}
}
