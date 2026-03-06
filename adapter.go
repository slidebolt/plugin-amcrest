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
	cfg      pluginConfig
	sink     runner.EventSink
	rawStore runner.RawStore

	clientFactory func(creds deviceCredentials) cameraClient

	mu        sync.RWMutex
	profiles  map[string]deviceCredentials
	loopStops map[string]context.CancelFunc
	loopsWG   sync.WaitGroup
}

func NewPluginAdapter() *PluginAdapter {
	p := &PluginAdapter{
		profiles:  make(map[string]deviceCredentials),
		loopStops: make(map[string]context.CancelFunc),
	}
	p.clientFactory = p.defaultClientFactory
	return p
}

func (p *PluginAdapter) defaultClientFactory(creds deviceCredentials) cameraClient {
	baseURL := fmt.Sprintf("%s://%s", creds.Scheme, creds.Host)
	return amcrest.NewClient(baseURL, creds.Username, creds.Password, nil)
}

func (p *PluginAdapter) OnInitialize(config runner.Config, state types.Storage) (types.Manifest, types.Storage) {
	p.cfg = loadPluginConfigFromEnv()
	p.sink = config.EventSink
	p.rawStore = config.RawStore

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
	}, state
}

func (p *PluginAdapter) OnReady() {}

func (p *PluginAdapter) WaitReady(ctx context.Context) error { return nil }

func (p *PluginAdapter) OnShutdown() {
	p.mu.Lock()
	for _, cancel := range p.loopStops {
		cancel()
	}
	p.loopStops = make(map[string]context.CancelFunc)
	p.mu.Unlock()
	p.loopsWG.Wait()
}

func (p *PluginAdapter) OnHealthCheck() (string, error) { return "perfect", nil }

func (p *PluginAdapter) OnStorageUpdate(current types.Storage) (types.Storage, error) {
	return current, nil
}

func (p *PluginAdapter) OnDeviceCreate(dev types.Device) (types.Device, error) {
	creds := parseDeviceCredentials(dev.Labels, p.cfg)
	if !creds.valid() {
		return dev, fmt.Errorf("missing required labels: %s, %s, %s", labelHost, labelUsername, labelPassword)
	}

	if dev.ID == "" {
		dev.ID = deviceIDForHost(creds.Host)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ConnectTimeout)
	defer cancel()

	client := p.clientFactory(creds)
	if info, err := client.GetSystemInfo(ctx); err != nil {
		return dev, fmt.Errorf("amcrest connect failed: %w", err)
	} else {
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
	creds.ConfigOptions = p.discoverConfigOptions(client)
	creds.Caps = p.discoverCapabilities(client, creds)
	creds.EventCodes = p.filterEventCodesByCapabilities(creds.EventCodes, creds.Caps)

	if err := p.writeDeviceCreds(dev.ID, creds); err != nil {
		return dev, err
	}
	p.storeCreds(dev.ID, creds)
	p.ensureEventLoop(dev.ID)

	dev.SourceID = creds.Host
	if creds.Serial != "" {
		dev.SourceID = creds.Serial
	}
	dev.SourceName = creds.sourceName()
	if dev.LocalName == "" {
		dev.LocalName = creds.localName()
	}
	dev.Labels = scrubSensitiveLabels(dev.Labels)

	return dev, nil
}

func (p *PluginAdapter) OnDeviceUpdate(dev types.Device) (types.Device, error) {
	return dev, nil
}

func (p *PluginAdapter) OnDeviceDelete(id string) error {
	p.stopEventLoop(id)
	return nil
}

func (p *PluginAdapter) OnDeviceSearch(q types.SearchQuery, res []types.Device) ([]types.Device, error) {
	return res, nil
}

func (p *PluginAdapter) OnDevicesList(current []types.Device) ([]types.Device, error) {
	for i := range current {
		if current[i].ID == pluginID {
			continue
		}
		if creds, ok := p.readOrCachedCreds(current[i].ID); ok {
			if current[i].SourceID == "" {
				current[i].SourceID = creds.Host
				if creds.Serial != "" {
					current[i].SourceID = creds.Serial
				}
			}
			if current[i].SourceName == "" {
				current[i].SourceName = creds.sourceName()
			}
			if current[i].LocalName == "" {
				current[i].LocalName = creds.localName()
			}
			p.ensureEventLoop(current[i].ID)
		}
	}
	return runner.EnsureCoreDevice(pluginID, current), nil
}

func (p *PluginAdapter) OnEntityCreate(e types.Entity) (types.Entity, error) { return e, nil }
func (p *PluginAdapter) OnEntityUpdate(e types.Entity) (types.Entity, error) { return e, nil }
func (p *PluginAdapter) OnEntityDelete(deviceID, entityID string) error      { return nil }

func (p *PluginAdapter) OnEntitiesList(deviceID string, current []types.Entity) ([]types.Entity, error) {
	if deviceID == pluginID {
		return runner.EnsureCoreEntities(pluginID, deviceID, current), nil
	}
	entities := []types.Entity{
		{ID: availabilityEntity, DeviceID: deviceID, Domain: "binary_sensor", LocalName: "Availability"},
	}
	if creds, ok := p.readOrCachedCreds(deviceID); ok {
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
			payload, _ := json.Marshal(map[string]any{
				"config":  cfg.Name,
				"enabled": cfg.Enabled,
			})
			entities = append(entities, types.Entity{
				ID:        "cfg-" + sanitizeEntityID(cfg.Name),
				DeviceID:  deviceID,
				Domain:    "binary_sensor",
				LocalName: fmt.Sprintf("Config: %s", cfg.Name),
				Data: types.EntityData{
					Reported: payload,
				},
			})
		}
	}
	return runner.EnsureCoreEntities(pluginID, deviceID, entities), nil
}

func (p *PluginAdapter) OnCommand(req types.Command, entity types.Entity) (types.Entity, error) {
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

	if p.sink != nil {
		payload, _ := json.Marshal(map[string]any{
			"type":        "updated",
			"format":      "jpeg",
			"size_bytes":  len(snapshot),
			"captured_at": time.Now().UTC().Format(time.RFC3339),
		})
		_ = p.sink.EmitEvent(types.InboundEvent{DeviceID: req.DeviceID, EntityID: snapshotEntityID, Payload: payload})
	}
	return entity, nil
}

func (p *PluginAdapter) OnEvent(evt types.Event, entity types.Entity) (types.Entity, error) {
	return entity, nil
}

func (p *PluginAdapter) ensureEventLoop(deviceID string) {
	if p.sink == nil {
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
	if p.sink == nil {
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
	payload, _ := json.Marshal(map[string]any{"type": eventType, "state": state})
	_ = p.sink.EmitEvent(types.InboundEvent{DeviceID: deviceID, EntityID: entityID, Payload: payload})
}

func (p *PluginAdapter) writeDeviceCreds(deviceID string, creds deviceCredentials) error {
	if p.rawStore == nil {
		return fmt.Errorf("raw store unavailable")
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	return p.rawStore.WriteRawDevice(deviceID, data)
}

func (p *PluginAdapter) readOrCachedCreds(deviceID string) (deviceCredentials, bool) {
	p.mu.RLock()
	cached, ok := p.profiles[deviceID]
	p.mu.RUnlock()
	if ok && cached.valid() {
		return cached, true
	}
	if p.rawStore == nil {
		return deviceCredentials{}, false
	}
	raw, err := p.rawStore.ReadRawDevice(deviceID)
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

func sanitizeEntityID(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	r := strings.NewReplacer(" ", "-", "_", "-", "/", "-", ".", "-", "&", "-", ":", "-", "(", "", ")", "")
	v = r.Replace(v)
	v = strings.Trim(v, "-")
	if v == "" {
		return "unknown"
	}
	return v
}
