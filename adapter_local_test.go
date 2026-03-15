//go:build local



package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/slidebolt/sdk-integration-testing"
	"github.com/slidebolt/sdk-types"
)

type localCamera struct {
	Host     string
	Username string
	Password string
}

func TestAmcrestLocal_CreateDevicesAndDiscoverEntities(t *testing.T) {
	cameras := requireAmcrestLocalEnv(t)

	s := integrationtesting.New(t, "github.com/slidebolt/plugin-amcrest", ".")
	s.RequirePlugin(pluginID)

	createdIDs := make([]string, 0, len(cameras))
	for i, cam := range cameras {
		requestID := fmt.Sprintf("amcrest-local-%d", i+1)
		dev := types.Device{
			ID: requestID,
			Labels: map[string][]string{
				labelHost:     {cam.Host},
				labelUsername: {cam.Username},
				labelPassword: {cam.Password},
				"site":        {"local"},
			},
		}
		created := createDevice(t, s, dev)
		if created.ID == "" {
			t.Fatalf("create device returned empty ID for request %q", requestID)
		}
		createdIDs = append(createdIDs, created.ID)
	}

	waitForDevices(t, s, createdIDs, 30*time.Second)
}

func TestAmcrestLocal_SnapshotCommand(t *testing.T) {
	cameras := requireAmcrestLocalEnv(t)
	if len(cameras) == 0 {
		t.Skip("no local Amcrest cameras configured")
	}

	s := integrationtesting.New(t, "github.com/slidebolt/plugin-amcrest", ".")
	s.RequirePlugin(pluginID)

	created := createDevice(t, s, types.Device{
		ID: "amcrest-local-snapshot",
		Labels: map[string][]string{
			labelHost:     {cameras[0].Host},
			labelUsername: {cameras[0].Username},
			labelPassword: {cameras[0].Password},
		},
	})
	devID := created.ID
	if devID == "" {
		t.Fatal("snapshot test device creation returned empty ID")
	}

	waitForDevices(t, s, []string{devID}, 30*time.Second)
	entities := getEntities(t, s, devID)
	hasSnapshot := false
	hasTrigger := false
	for _, e := range entities {
		if e.ID == snapshotEntityID {
			hasSnapshot = true
		}
		if e.ID == snapshotButtonID {
			hasTrigger = true
		}
	}
	if !hasSnapshot || !hasTrigger {
		t.Skipf("camera did not expose snapshot entities (snapshot=%v trigger=%v)", hasSnapshot, hasTrigger)
	}

	cmdID := postCommandAndWait(t, s, pluginID, devID, snapshotButtonID, map[string]any{"type": "press"}, 10*time.Second)
	waitForCommandState(t, s, pluginID, cmdID, types.CommandSucceeded, 20*time.Second)
}

func createDevice(t *testing.T, s *integrationtesting.Suite, dev types.Device) types.Device {
	t.Helper()
	raw := postJSON(t, s, "/api/plugins/"+pluginID+"/devices", dev)
	var created types.Device
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("create device: decode response: %v (body=%s)", err, string(raw))
	}
	return created
}

func requireAmcrestLocalEnv(t *testing.T) []localCamera {
	t.Helper()
	cams := collectLocalCameras()
	if len(cams) == 0 {
		t.Skip("no local camera env found; source plugin-amcrest/.env.local before running local tests")
	}
	return cams
}

func collectLocalCameras() []localCamera {
	var cams []localCamera
	for i := 1; i <= 16; i++ {
		host := strings.TrimSpace(getenvFirst(
			fmt.Sprintf("AMCREST_TEST_HOST_%d", i),
			fmt.Sprintf("AMCREST_TEST_IP_%d", i),
		))
		user := strings.TrimSpace(getenvFirst(
			fmt.Sprintf("AMCREST_TEST_USERNAME_%d", i),
			fmt.Sprintf("AMCREST_TEST_USER_%d", i),
		))
		pass := strings.TrimSpace(getenvFirst(
			fmt.Sprintf("AMCREST_TEST_PASSWORD_%d", i),
			fmt.Sprintf("AMCREST_TEST_PASS_%d", i),
		))
		if host == "" {
			continue
		}
		if user == "" || pass == "" {
			continue
		}
		cams = append(cams, localCamera{Host: host, Username: user, Password: pass})
	}

	if len(cams) == 0 {
		host := strings.TrimSpace(getenvFirst("AMCREST_TEST_HOST", "AMCREST_TEST_IP"))
		user := strings.TrimSpace(getenvFirst("AMCREST_TEST_USERNAME", "AMCREST_TEST_USER"))
		pass := strings.TrimSpace(getenvFirst("AMCREST_TEST_PASSWORD", "AMCREST_TEST_PASS"))
		if host != "" && user != "" && pass != "" {
			cams = append(cams, localCamera{Host: host, Username: user, Password: pass})
		}
	}
	return cams
}

func getenvFirst(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func postJSON(t *testing.T, s *integrationtesting.Suite, path string, body any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("postJSON marshal: %v", err)
	}
	resp, err := http.Post(s.APIURL()+path, "application/json", bytes.NewReader(b)) //nolint:noctx
	if err != nil {
		t.Fatalf("postJSON %s: %v", path, err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("postJSON %s: status %d body=%s", path, resp.StatusCode, raw.String())
	}
	return raw.Bytes()
}

func getEntities(t *testing.T, s *integrationtesting.Suite, deviceID string) []types.Entity {
	t.Helper()
	path := fmt.Sprintf("/api/plugins/%s/devices/%s/entities", pluginID, deviceID)
	var entities []types.Entity
	if err := s.GetJSON(path, &entities); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return entities
}

func waitForDevices(t *testing.T, s *integrationtesting.Suite, ids []string, timeout time.Duration) {
	t.Helper()
	ok := s.WaitFor(timeout, func() bool {
		var devices []types.Device
		if err := s.GetJSON("/api/plugins/"+pluginID+"/devices", &devices); err != nil {
			return false
		}
		found := map[string]bool{}
		for _, d := range devices {
			found[d.ID] = true
		}
		for _, id := range ids {
			if !found[id] {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("did not discover expected devices %v within %s", ids, timeout)
	}
}

func postCommandAndWait(t *testing.T, s *integrationtesting.Suite, pluginID, deviceID, entityID string, payload map[string]any, timeout time.Duration) string {
	t.Helper()
	path := fmt.Sprintf("%s/api/plugins/%s/devices/%s/entities/%s/commands", s.APIURL(), pluginID, deviceID, entityID)
	body, _ := json.Marshal(payload)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Post(path, "application/json", bytes.NewReader(body)) //nolint:noctx
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var st types.CommandStatus
		_ = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
			return st.CommandID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("command to %s/%s/%s was not accepted within %s", pluginID, deviceID, entityID, timeout)
	return ""
}

func waitForCommandState(t *testing.T, s *integrationtesting.Suite, pluginID, cmdID string, want types.CommandState, timeout time.Duration) {
	t.Helper()
	path := fmt.Sprintf("/api/plugins/%s/commands/%s", pluginID, cmdID)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var st types.CommandStatus
		if err := s.GetJSON(path, &st); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if st.State == want {
			return
		}
		if st.State == types.CommandFailed {
			t.Fatalf("command %s failed: %s", cmdID, st.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("command %s did not reach %q within %s", cmdID, want, timeout)
}
