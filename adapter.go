package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slidebolt/plugin-amcrest/pkg/amcrest"
	runner "github.com/slidebolt/sdk-runner"
	"github.com/slidebolt/sdk-types"
)

const (
	pluginID           = "plugin-amcrest"
	domainAmcrest      = "camera.amcrest"
	snapshotEntityID   = "snapshot"
	snapshotButtonID   = "snapshot_trigger"
	streamEntityID     = "stream_main"
	doorbellEntityID   = "doorbell_press"
	motionEntityID     = "motion"
	availabilityEntity = "availability"
)

type cameraClient interface {
	GetSystemInfo(ctx context.Context) (map[string]string, error)
	GetSoftwareVersion(ctx context.Context) (string, error)
	GetSnapshot(ctx context.Context) ([]byte, error)
	GetEventCodes(ctx context.Context) ([]string, error)
	GetAllConfig(ctx context.Context) (map[string]string, error)
	SubscribeEvents(ctx context.Context, codes []string, onEvent func(amcrest.Event)) error
}

type PluginAdapter struct {
	cfg  pluginConfig
	pctx runner.PluginContext

	// Optional compatibility hook used by tests that still verify raw-device persistence.
	rawStore deviceRawStore

	clientFactory func(creds deviceCredentials) cameraClient

	mu        sync.RWMutex
	profiles  map[string]deviceCredentials
	loopStops map[string]context.CancelFunc
	loopsWG   sync.WaitGroup
	seen      map[string]struct{}

	runCtx    context.Context
	runCancel context.CancelFunc
	runWG     sync.WaitGroup
}

type deviceRawStore interface {
	ReadRawDevice(deviceID string) (json.RawMessage, error)
	WriteRawDevice(deviceID string, data json.RawMessage) error
}

func NewPluginAdapter() *PluginAdapter {
	p := &PluginAdapter{
		profiles:  make(map[string]deviceCredentials),
		loopStops: make(map[string]context.CancelFunc),
		seen:      make(map[string]struct{}),
	}
	p.clientFactory = p.defaultClientFactory
	return p
}

func (p *PluginAdapter) defaultClientFactory(creds deviceCredentials) cameraClient {
	baseURL := fmt.Sprintf("%s://%s", creds.Scheme, creds.Host)
	return amcrest.NewClient(baseURL, creds.Username, creds.Password, nil)
}

func (p *PluginAdapter) Initialize(ctx runner.PluginContext) (types.Manifest, error) {
	p.cfg = loadPluginConfigFromEnv()
	p.pctx = ctx

	schemas := append([]types.DomainDescriptor{}, types.CoreDomains()...)
	schemas = append(schemas, types.DomainDescriptor{
		Domain: domainAmcrest,
		Commands: []types.ActionDescriptor{
			{Action: "capture_snapshot"},
			{Action: "refresh_stream"},
		},
		Events: []types.ActionDescriptor{
			{Action: "doorbell_pressed"},
			{Action: "motion_detected"},
		},
	})

	return types.Manifest{
		ID:      pluginID,
		Name:    "Amcrest Plugin",
		Version: "0.2.0",
		Schemas: schemas,
	}, nil
}

func (p *PluginAdapter) Start(_ context.Context) error {
	if p.pctx.Registry == nil {
		return nil
	}
	p.runCtx, p.runCancel = context.WithCancel(context.Background())
	p.ensureCoreState()
	p.reconcileAllDevices()
	p.runWG.Add(1)
	go func() {
		defer p.runWG.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.runCtx.Done():
				return
			case <-t.C:
				p.reconcileAllDevices()
			}
		}
	}()
	p.runWG.Add(1)
	go func() {
		defer p.runWG.Done()
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.runCtx.Done():
				return
			case <-t.C:
				p.reconcileNewDevices()
			}
		}
	}()
	return nil
}

func (p *PluginAdapter) Stop() error {
	if p.runCancel != nil {
		p.runCancel()
		p.runWG.Wait()
	}
	p.mu.Lock()
	for _, cancel := range p.loopStops {
		cancel()
	}
	p.loopStops = make(map[string]context.CancelFunc)
	p.mu.Unlock()
	p.loopsWG.Wait()
	return nil
}

func (p *PluginAdapter) OnReset() error {
	_ = p.Stop()
	if p.pctx.Registry == nil {
		return nil
	}
	for _, dev := range p.pctx.Registry.LoadDevices() {
		_ = p.pctx.Registry.DeleteDevice(dev.ID)
	}
	return p.pctx.Registry.DeleteState()
}

func (p *PluginAdapter) OnHealthCheck() (string, error) { return "perfect", nil }

func (p *PluginAdapter) runCommand(req types.Command, entity types.Entity) (types.Entity, error) {
	if entity.ID != snapshotButtonID {
		return entity, nil
	}
	creds, ok := p.readOrCachedCreds(req.DeviceID)
	if !ok || !creds.valid() {
		return entity, fmt.Errorf("missing amcrest credentials for device %s", req.DeviceID)
	}
	if !creds.Caps.Snapshot {
		return entity, fmt.Errorf("device %s does not support snapshots", req.DeviceID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := p.clientFactory(creds).GetSnapshot(ctx)
	if err != nil {
		return entity, err
	}

	if p.pctx.Events != nil {
		payload, _ := json.Marshal(map[string]any{
			"type":        "updated",
			"format":      "jpeg",
			"size_bytes":  len(snapshot),
			"captured_at": time.Now().UTC().Format(time.RFC3339),
		})
		_ = p.pctx.Events.PublishEvent(types.InboundEvent{DeviceID: req.DeviceID, EntityID: snapshotEntityID, Payload: payload})
	}
	return entity, nil
}

func (p *PluginAdapter) OnCommand(req types.Command, entity types.Entity) error {
	updated, err := p.runCommand(req, entity)
	if p.pctx.Registry != nil && updated.ID != "" && updated.DeviceID != "" {
		_ = p.pctx.Registry.SaveEntity(updated)
	}
	return err
}

func (p *PluginAdapter) ensureEventLoop(deviceID string) {
	if p.pctx.Events == nil {
		return
	}
	creds, ok := p.readOrCachedCreds(deviceID)
	if !ok || !creds.valid() {
		return
	}
	if len(creds.EventCodes) == 0 {
		return
	}

	p.mu.Lock()
	if _, exists := p.loopStops[deviceID]; exists {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.loopStops[deviceID] = cancel
	p.mu.Unlock()

	p.loopsWG.Add(1)
	go func() {
		defer p.loopsWG.Done()
		defer p.stopEventLoop(deviceID)
		p.runEventLoop(ctx, deviceID, creds)
	}()
}

func (p *PluginAdapter) ensureCoreState() {
	if p.pctx.Registry == nil {
		return
	}
	coreID := types.CoreDeviceID(pluginID)
	_ = p.pctx.Registry.SaveDevice(types.Device{
		ID:         coreID,
		SourceID:   coreID,
		SourceName: pluginID,
		LocalName:  pluginID,
	})
	for _, ent := range types.CoreEntities(pluginID) {
		_ = p.pctx.Registry.SaveEntity(ent)
	}
}

func (p *PluginAdapter) reconcileAllDevices() {
	if p.pctx.Registry == nil {
		return
	}
	devices := p.pctx.Registry.LoadDevices()
	for _, dev := range devices {
		if dev.ID == types.CoreDeviceID(pluginID) {
			continue
		}
		updated, creds, ok := p.resolveAndHydrateDevice(dev)
		if !ok {
			continue
		}
		_ = p.pctx.Registry.SaveDevice(updated)
		p.syncEntitiesForDevice(updated.ID, creds)
		p.ensureEventLoop(updated.ID)
		p.markSeen(updated.ID)
	}
}

func (p *PluginAdapter) reconcileNewDevices() {
	if p.pctx.Registry == nil {
		return
	}
	for _, dev := range p.pctx.Registry.LoadDevices() {
		if dev.ID == types.CoreDeviceID(pluginID) {
			continue
		}
		if p.isSeen(dev.ID) {
			continue
		}
		updated, creds, ok := p.resolveAndHydrateDevice(dev)
		if !ok {
			continue
		}
		_ = p.pctx.Registry.SaveDevice(updated)
		p.syncEntitiesForDevice(updated.ID, creds)
		p.ensureEventLoop(updated.ID)
		p.markSeen(updated.ID)
	}
}

func (p *PluginAdapter) markSeen(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[deviceID] = struct{}{}
}

func (p *PluginAdapter) isSeen(deviceID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.seen[deviceID]
	return ok
}

func (p *PluginAdapter) resolveAndHydrateDevice(dev types.Device) (types.Device, deviceCredentials, bool) {
	if dev.ID == "" {
		return dev, deviceCredentials{}, false
	}

	creds := parseDeviceCredentials(dev.Labels, p.cfg)
	if !creds.valid() {
		stored, ok := p.readOrCachedCreds(dev.ID)
		if !ok {
			return dev, deviceCredentials{}, false
		}
		creds = stored
	} else {
		if dev.ID == "" {
			dev.ID = deviceIDForHost(creds.Host)
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ConnectTimeout)
		client := p.clientFactory(creds)
		if info, err := getSystemInfoWithTimeout(ctx, client); err == nil {
			if model := strings.TrimSpace(info["deviceType"]); model != "" {
				creds.Model = model
			}
			if serial := strings.TrimSpace(info["serialNumber"]); serial != "" {
				creds.Serial = serial
			}
		}
		if version, err := client.GetSoftwareVersion(ctx); err == nil {
			creds.Version = strings.TrimSpace(version)
		}
		cancel()
		creds.ConfigOptions = p.discoverConfigOptions(client)
		creds.Caps = p.discoverCapabilities(client, creds)
		creds.EventCodes = p.filterEventCodesByCapabilities(creds.EventCodes, creds.Caps)
		if err := p.writeDeviceCreds(dev.ID, creds); err != nil {
			return dev, deviceCredentials{}, false
		}
	}

	p.storeCreds(dev.ID, creds)
	dev.SourceID = creds.Host
	if creds.Serial != "" {
		dev.SourceID = creds.Serial
	}
	dev.SourceName = creds.sourceName()
	if strings.TrimSpace(dev.LocalName) == "" {
		dev.LocalName = creds.localName()
	}
	dev.Labels = scrubSensitiveLabels(dev.Labels)
	return dev, creds, true
}

func (p *PluginAdapter) syncEntitiesForDevice(deviceID string, creds deviceCredentials) {
	if p.pctx.Registry == nil {
		return
	}
	want := map[string]types.Entity{}
	for _, ent := range buildEntitiesForDevice(deviceID, creds) {
		want[ent.ID] = ent
		_ = p.pctx.Registry.SaveEntity(ent)
	}
	current := p.pctx.Registry.GetEntities(p.pctx.Registry.Namespace(), deviceID)
	for _, ent := range current {
		if _, keep := want[ent.ID]; keep {
			continue
		}
		_ = p.pctx.Registry.DeleteEntity(p.pctx.Registry.Namespace(), deviceID, ent.ID)
	}
}

func buildEntitiesForDevice(deviceID string, creds deviceCredentials) []types.Entity {
	entities := []types.Entity{
		{ID: availabilityEntity, DeviceID: deviceID, Domain: "binary_sensor", LocalName: "Availability"},
	}
	if creds.Caps.StreamMain {
		entities = append(entities, types.Entity{ID: streamEntityID, DeviceID: deviceID, Domain: "stream", LocalName: "Main Stream"})
	}
	if creds.Caps.Snapshot {
		entities = append(entities,
			types.Entity{ID: snapshotEntityID, DeviceID: deviceID, Domain: "image", LocalName: "Snapshot"},
			types.Entity{ID: snapshotButtonID, DeviceID: deviceID, Domain: "button", LocalName: "Capture Snapshot", Actions: []string{"press"}},
		)
	}
	if creds.Caps.Motion {
		entities = append(entities, types.Entity{ID: motionEntityID, DeviceID: deviceID, Domain: "binary_sensor", LocalName: "Motion"})
	}
	if creds.Caps.DoorbellPress {
		entities = append(entities, types.Entity{ID: doorbellEntityID, DeviceID: deviceID, Domain: "binary_sensor", LocalName: "Doorbell Press"})
	}
	for _, cfg := range creds.ConfigOptions {
		payload, _ := json.Marshal(map[string]any{"config": cfg.Name, "enabled": cfg.Enabled})
		entities = append(entities, types.Entity{
			ID:        "cfg-" + sanitizeEntityID(cfg.Name),
			DeviceID:  deviceID,
			Domain:    "binary_sensor",
			LocalName: fmt.Sprintf("Config: %s", cfg.Name),
			Data:      types.EntityData{Reported: payload},
		})
	}
	return entities
}

func (p *PluginAdapter) stopEventLoop(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cancel, ok := p.loopStops[deviceID]
	if !ok {
		return
	}
	delete(p.loopStops, deviceID)
	cancel()
}

func (p *PluginAdapter) runEventLoop(ctx context.Context, deviceID string, creds deviceCredentials) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := p.clientFactory(creds).SubscribeEvents(ctx, creds.EventCodes, func(ev amcrest.Event) {
			p.handleEvent(deviceID, ev)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("plugin-amcrest: event stream error for device %s host %s: %v", deviceID, creds.Host, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (p *PluginAdapter) handleEvent(deviceID string, ev amcrest.Event) {
	if p.pctx.Events == nil {
		return
	}
	state := "on"
	if strings.EqualFold(ev.Action, "stop") {
		state = "off"
	}
	switch ev.Code {
	case "_DoorbellPress_":
		p.emitBinaryState(deviceID, doorbellEntityID, state, "doorbell_pressed")
	case "VideoMotion":
		p.emitBinaryState(deviceID, motionEntityID, state, "motion_detected")
	}
}

func (p *PluginAdapter) emitBinaryState(deviceID, entityID, state, eventType string) {
	if p.pctx.Events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"type": eventType, "state": state})
	_ = p.pctx.Events.PublishEvent(types.InboundEvent{DeviceID: deviceID, EntityID: entityID, Payload: payload})
}

func (p *PluginAdapter) writeDeviceCreds(deviceID string, creds deviceCredentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	if p.rawStore != nil {
		return p.rawStore.WriteRawDevice(deviceID, data)
	}
	if p.pctx.Registry == nil {
		return fmt.Errorf("raw store unavailable")
	}
	state := p.loadPluginState()
	if state.DeviceCreds == nil {
		state.DeviceCreds = map[string]json.RawMessage{}
	}
	state.DeviceCreds[deviceID] = data
	return p.savePluginState(state)
}

func (p *PluginAdapter) readOrCachedCreds(deviceID string) (deviceCredentials, bool) {
	p.mu.RLock()
	cached, ok := p.profiles[deviceID]
	p.mu.RUnlock()
	if ok && cached.valid() {
		return cached, true
	}
	raw, err := p.readDeviceCredsRaw(deviceID)
	if err != nil || len(raw) == 0 {
		return deviceCredentials{}, false
	}
	var creds deviceCredentials
	if err := json.Unmarshal(raw, &creds); err != nil || !creds.valid() {
		return deviceCredentials{}, false
	}
	if len(creds.EventCodes) == 0 {
		creds.EventCodes = append([]string(nil), p.cfg.DefaultEventCodes...)
	}
	if len(creds.ConfigOptions) == 0 || !creds.Caps.any() {
		// If capabilities were not persisted, run discovery once and persist.
		client := p.clientFactory(creds)
		if len(creds.ConfigOptions) == 0 {
			creds.ConfigOptions = p.discoverConfigOptions(client)
		}
		creds.Caps = p.discoverCapabilities(client, creds)
		creds.EventCodes = p.filterEventCodesByCapabilities(creds.EventCodes, creds.Caps)
		_ = p.writeDeviceCreds(deviceID, creds)
	}
	p.storeCreds(deviceID, creds)
	return creds, true
}

func (p *PluginAdapter) readDeviceCredsRaw(deviceID string) (json.RawMessage, error) {
	if p.rawStore != nil {
		return p.rawStore.ReadRawDevice(deviceID)
	}
	if p.pctx.Registry == nil {
		return nil, fmt.Errorf("registry unavailable")
	}
	state := p.loadPluginState()
	raw := state.DeviceCreds[deviceID]
	if len(raw) == 0 {
		return nil, fmt.Errorf("not found")
	}
	return raw, nil
}

func (p *PluginAdapter) storeCreds(deviceID string, creds deviceCredentials) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.profiles[deviceID] = creds
}

func deviceIDForHost(host string) string {
	id := strings.ToLower(strings.TrimSpace(host))
	replacer := strings.NewReplacer(":", "-", ".", "-", "/", "-", "_", "-")
	id = replacer.Replace(id)
	id = strings.Trim(id, "-")
	if id == "" {
		id = "unknown"
	}
	return "amcrest-" + id
}

func (d deviceCredentials) localName() string {
	if d.Model != "" {
		return fmt.Sprintf("%s (%s)", d.Model, d.Host)
	}
	return d.Host
}

func (d deviceCredentials) sourceName() string {
	if d.Version != "" && d.Model != "" {
		return fmt.Sprintf("%s %s", d.Model, d.Version)
	}
	if d.Model != "" {
		return d.Model
	}
	return "Amcrest Camera"
}

func (p *PluginAdapter) discoverCapabilities(client cameraClient, creds deviceCredentials) deviceCapabilities {
	caps := deviceCapabilities{
		StreamMain: true,
	}
	snapCtx, snapCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if _, err := client.GetSnapshot(snapCtx); err == nil {
		caps.Snapshot = true
	}
	snapCancel()

	eventsCtx, eventsCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if codes, err := client.GetEventCodes(eventsCtx); err == nil {
		for _, code := range codes {
			switch strings.TrimSpace(code) {
			case "_DoorbellPress_":
				caps.DoorbellPress = true
			case "VideoMotion":
				caps.Motion = true
			}
		}
	}
	eventsCancel()

	// Heuristic fallback: explicit doorbell models should expose doorbell events.
	model := strings.ToLower(strings.TrimSpace(creds.Model))
	if !caps.DoorbellPress && (strings.Contains(model, "doorbell") || strings.Contains(model, "ad410")) {
		caps.DoorbellPress = true
	}
	return caps
}

func (p *PluginAdapter) discoverConfigOptions(client cameraClient) []configOption {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	kv, err := client.GetAllConfig(ctx)
	if err != nil {
		return nil
	}
	enabledBySection := map[string]bool{}
	hasEnableBySection := map[string]bool{}
	sections := map[string]struct{}{}
	for key, value := range kv {
		if !strings.HasPrefix(key, "table.All.") {
			continue
		}
		rest := strings.TrimPrefix(key, "table.All.")
		section := rest
		if idx := strings.IndexAny(section, ".["); idx >= 0 {
			section = section[:idx]
		}
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		sections[section] = struct{}{}

		if strings.HasSuffix(rest, ".Enable") || strings.Contains(rest, "].Enable") {
			hasEnableBySection[section] = true
			if strings.EqualFold(strings.TrimSpace(value), "true") {
				enabledBySection[section] = true
			}
		}
	}
	if len(sections) == 0 {
		return nil
	}
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]configOption, 0, len(names))
	for _, name := range names {
		enabled := false
		if hasEnableBySection[name] {
			enabled = enabledBySection[name]
		}
		out = append(out, configOption{Name: name, Enabled: enabled})
	}
	return out
}

func (p *PluginAdapter) filterEventCodesByCapabilities(eventCodes []string, caps deviceCapabilities) []string {
	if len(eventCodes) == 0 {
		return nil
	}
	out := make([]string, 0, len(eventCodes))
	for _, code := range eventCodes {
		switch code {
		case "_DoorbellPress_":
			if caps.DoorbellPress {
				out = append(out, code)
			}
		case "VideoMotion":
			if caps.Motion {
				out = append(out, code)
			}
		default:
			out = append(out, code)
		}
	}
	return out
}

func (c deviceCapabilities) any() bool {
	return c.Snapshot || c.StreamMain || c.DoorbellPress || c.Motion
}

func getSystemInfoWithTimeout(ctx context.Context, client cameraClient) (map[string]string, error) {
	type result struct {
		info map[string]string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := client.GetSystemInfo(ctx)
		done <- result{info: info, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-done:
		return out.info, out.err
	}
}

func sanitizeEntityID(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(v))
	lastDash := false
	for i := 0; i < len(v); i++ {
		ch := v[i]
		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if isAlphaNum {
			b.WriteByte(ch)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

type pluginState struct {
	DeviceCreds map[string]json.RawMessage `json:"device_creds,omitempty"`
}

func (p *PluginAdapter) loadPluginState() pluginState {
	if p.pctx.Registry == nil {
		return pluginState{}
	}
	raw, ok := p.pctx.Registry.LoadState()
	if !ok || len(raw.Data) == 0 {
		return pluginState{DeviceCreds: map[string]json.RawMessage{}}
	}
	var s pluginState
	if err := json.Unmarshal(raw.Data, &s); err != nil {
		return pluginState{DeviceCreds: map[string]json.RawMessage{}}
	}
	if s.DeviceCreds == nil {
		s.DeviceCreds = map[string]json.RawMessage{}
	}
	return s
}

func (p *PluginAdapter) savePluginState(s pluginState) error {
	if p.pctx.Registry == nil {
		return fmt.Errorf("registry unavailable")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return p.pctx.Registry.SaveState(types.Storage{Data: raw})
}
