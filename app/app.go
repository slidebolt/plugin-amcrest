// plugin-amcrest integrates Amcrest IP cameras with SlideBolt.
//
// Features:
//   - HTTP Digest authentication
//   - Camera entity discovery from configuration
//   - Snapshot command for fetching JPEG images
//   - Device info and event code querying
package app

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	contract "github.com/slidebolt/sb-contract"
	domain "github.com/slidebolt/sb-domain"
	messenger "github.com/slidebolt/sb-messenger-sdk"
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

type CameraSnapshot struct{}

func (CameraSnapshot) ActionName() string { return "camera_snapshot" }

func init() {
	domain.RegisterCommand("camera_snapshot", CameraSnapshot{})
}

type AmcrestClient struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
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

type App struct {
	msg     messenger.Messenger
	store   storage.Storage
	cmds    *messenger.Commands
	subs    []messenger.Subscription
	clients map[string]*AmcrestClient
}

func New() *App {
	return &App{}
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
	msg, err := messenger.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect messenger: %w", err)
	}
	a.msg = msg

	store, err := storage.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect storage: %w", err)
	}
	a.store = store

	a.clients = make(map[string]*AmcrestClient)
	a.cmds = messenger.NewCommands(msg, domain.LookupCommand)
	sub, err := a.cmds.Receive(PluginID+".>", a.handleCommand)
	if err != nil {
		return nil, fmt.Errorf("subscribe commands: %w", err)
	}
	a.subs = append(a.subs, sub)

	if err := a.reconcileStoredCameras(); err != nil {
		log.Printf("plugin-amcrest: camera discovery error: %v", err)
	}

	log.Println("plugin-amcrest: started")
	return nil, nil
}

func (a *App) OnShutdown() error {
	for _, sub := range a.subs {
		sub.Unsubscribe()
	}
	if a.store != nil {
		a.store.Close()
	}
	if a.msg != nil {
		a.msg.Close()
	}
	return nil
}

func (a *App) reconcileStoredCameras() error {
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

		cfg, err := cameraConfigFromDevice(device)
		if err != nil {
			log.Printf("plugin-amcrest: invalid camera config for %s: %v", device.ID, err)
			continue
		}

		client := newClientFromConfig(cfg)
		a.clients[cfg.ID] = client

		state := domain.Camera{}
		meta := map[string]json.RawMessage{
			"connected": json.RawMessage(`false`),
		}
		deviceInfo, err := client.GetSystemInfo(ctx)
		if err != nil {
			errJSON, _ := json.Marshal(err.Error())
			meta["last_error"] = json.RawMessage(errJSON)
			log.Printf("plugin-amcrest: failed to connect to camera %s: %v", cfg.ID, err)
		} else {
			meta["connected"] = json.RawMessage(`true`)
			if infoJSON, err := json.Marshal(deviceInfo); err == nil {
				meta["device_info"] = json.RawMessage(infoJSON)
			}
		}
		version, err := client.GetSoftwareVersion(ctx)
		if err == nil {
			if vJSON, err := json.Marshal(version); err == nil {
				meta["version"] = json.RawMessage(vJSON)
			}
		}
		eventCodes, err := client.GetEventCodes(ctx)
		if err == nil {
			if ecJSON, err := json.Marshal(eventCodes); err == nil {
				meta["event_codes"] = json.RawMessage(ecJSON)
			}
			state.MotionDetection = containsMotionEvent(eventCodes)
		}

		entity, err := a.loadStoredCameraEntity(cfg.ID)
		if err != nil {
			entity = domain.Entity{
				ID:       cfg.ID,
				Plugin:   PluginID,
				DeviceID: cfg.ID,
				Type:     "camera",
				Name:     device.Name,
				Commands: []string{"camera_snapshot"},
			}
			if entity.Name == "" {
				entity.Name = fmt.Sprintf("Amcrest Camera %s", cfg.Host)
			}
		}
		entity.State = state
		entity.Meta = meta
		if len(entity.Commands) == 0 {
			entity.Commands = []string{"camera_snapshot"}
		}
		if err := a.store.Save(entity); err != nil {
			log.Printf("plugin-amcrest: failed to save camera entity %s: %v", cfg.ID, err)
		} else {
			log.Printf("plugin-amcrest: registered camera %s (connected=%s)", cfg.ID, meta["connected"])
		}
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

func cameraConfigFromDevice(device domain.Device) (CameraConfig, error) {
	cfg := CameraConfig{ID: device.ID}

	if cfg.ID == "" {
		return cfg, fmt.Errorf("missing device id")
	}
	if err := unmarshalMetaString(device.Meta, "host", &cfg.Host); err != nil {
		return cfg, err
	}
	if err := unmarshalMetaString(device.Meta, "username", &cfg.Username); err != nil {
		return cfg, err
	}
	if err := unmarshalMetaString(device.Meta, "password", &cfg.Password); err != nil {
		return cfg, err
	}
	_ = unmarshalMetaString(device.Meta, "scheme", &cfg.Scheme)
	_ = unmarshalMetaInt(device.Meta, "port", &cfg.Port)
	_ = unmarshalMetaInt(device.Meta, "timeout_ms", &cfg.Timeout)

	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return cfg, fmt.Errorf("device meta must include host, username, password")
	}
	return cfg, nil
}

func unmarshalMetaString(meta map[string]json.RawMessage, key string, dest *string) error {
	if len(meta) == 0 {
		return fmt.Errorf("missing meta.%s", key)
	}
	raw, ok := meta[key]
	if !ok {
		return fmt.Errorf("missing meta.%s", key)
	}
	return json.Unmarshal(raw, dest)
}

func unmarshalMetaInt(meta map[string]json.RawMessage, key string, dest *int) error {
	if len(meta) == 0 {
		return fmt.Errorf("missing meta.%s", key)
	}
	raw, ok := meta[key]
	if !ok {
		return fmt.Errorf("missing meta.%s", key)
	}
	return json.Unmarshal(raw, dest)
}

func (a *App) loadStoredCameraEntity(cameraID string) (domain.Entity, error) {
	raw, err := a.store.Get(domain.EntityKey{Plugin: PluginID, DeviceID: cameraID, ID: cameraID})
	if err != nil {
		return domain.Entity{}, err
	}
	var entity domain.Entity
	if err := json.Unmarshal(raw, &entity); err != nil {
		return domain.Entity{}, err
	}
	return entity, nil
}

func (a *App) handleCommand(addr messenger.Address, cmd any) {
	cameraID := addr.EntityID
	switch cmd.(type) {
	case CameraSnapshot:
		a.handleSnapshot(cameraID, addr)
	default:
		log.Printf("plugin-amcrest: unknown command %T for %s", cmd, addr.Key())
	}
}

func (a *App) handleSnapshot(cameraID string, addr messenger.Address) {
	client, ok := a.clients[cameraID]
	if !ok {
		log.Printf("plugin-amcrest: camera %s not found", cameraID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snapshot, err := client.GetSnapshot(ctx)
	if err != nil {
		log.Printf("plugin-amcrest: snapshot failed for %s: %v", cameraID, err)
		a.updateCameraMeta(cameraID, func(meta map[string]json.RawMessage) {
			errJSON, _ := json.Marshal(err.Error())
			meta["last_error"] = json.RawMessage(errJSON)
		})
		return
	}

	log.Printf("plugin-amcrest: snapshot for %s: %d bytes", cameraID, len(snapshot))
	a.updateCameraMeta(cameraID, func(meta map[string]json.RawMessage) {
		delete(meta, "last_error")
		meta["last_snapshot_bytes"] = json.RawMessage(fmt.Sprintf(`%d`, len(snapshot)))
	})
}

func (a *App) updateCameraMeta(cameraID string, update func(map[string]json.RawMessage)) {
	eKey := domain.EntityKey{Plugin: PluginID, DeviceID: cameraID, ID: cameraID}
	raw, err := a.store.Get(eKey)
	if err != nil {
		log.Printf("plugin-amcrest: failed to get camera %s: %v", cameraID, err)
		return
	}

	var entity domain.Entity
	if err := json.Unmarshal(raw, &entity); err != nil {
		log.Printf("plugin-amcrest: failed to unmarshal camera %s: %v", cameraID, err)
		return
	}

	if entity.Meta == nil {
		entity.Meta = make(map[string]json.RawMessage)
	}
	update(entity.Meta)
	if err := a.store.Save(entity); err != nil {
		log.Printf("plugin-amcrest: failed to update camera meta %s: %v", cameraID, err)
	}
}

func containsMotionEvent(codes []string) bool {
	for _, c := range codes {
		if c == "VideoMotion" {
			return true
		}
	}
	return false
}
