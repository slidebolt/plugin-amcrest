package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	domain "github.com/slidebolt/sb-domain"
	testkit "github.com/slidebolt/sb-testkit"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
)

func env(t *testing.T) (*testkit.TestEnv, storage.Storage, *messenger.Commands) {
	t.Helper()
	e := testkit.NewTestEnv(t)
	e.Start("messenger")
	e.Start("storage")
	cmds := messenger.NewCommands(e.Messenger(), domain.LookupCommand)
	return e, e.Storage(), cmds
}

func saveEntity(t *testing.T, store storage.Storage, plugin, device, id, typ, name string, state any) domain.Entity {
	t.Helper()
	e := domain.Entity{ID: id, Plugin: plugin, DeviceID: device, Type: typ, Name: name, State: state}
	if err := store.Save(e); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
	return e
}

func queryByType(t *testing.T, store storage.Storage, typ string) []storage.Entry {
	t.Helper()
	entries, err := store.Query(storage.Query{Where: []storage.Filter{{Field: "type", Op: storage.Eq, Value: typ}}})
	if err != nil {
		t.Fatalf("query type=%s: %v", typ, err)
	}
	return entries
}

func TestCameraSnapshotCommandRegistration(t *testing.T) {
	cmdType, ok := domain.LookupCommand("camera_snapshot")
	if !ok {
		t.Fatal("camera_snapshot command not registered")
	}
	if cmdType.Kind() != 25 {
		t.Fatalf("expected struct, got %v", cmdType.Kind())
	}
}

func TestPluginHello(t *testing.T) {
	hello := New().Hello()
	if hello.ID != PluginID {
		t.Errorf("ID: got %q, want %q", hello.ID, PluginID)
	}
	if hello.Kind != "plugin" {
		t.Errorf("Kind: got %q, want %q", hello.Kind, "plugin")
	}
	if len(hello.DependsOn) != 2 {
		t.Errorf("DependsOn: got %v, want [messenger storage]", hello.DependsOn)
	}
}

func TestCameraEntitySaveAndRetrieve(t *testing.T) {
	_, store, _ := env(t)
	state := domain.Camera{
		MotionDetection: true,
	}
	e := saveEntity(t, store, PluginID, "cam1", "cam1", "camera", "Test Camera", state)
	entries := queryByType(t, store, "camera")
	if len(entries) != 1 {
		t.Fatalf("expected 1 camera, got %d", len(entries))
	}
	var retrieved domain.Entity
	if err := json.Unmarshal(entries[0].Data, &retrieved); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if retrieved.ID != e.ID {
		t.Errorf("ID: got %q, want %q", retrieved.ID, e.ID)
	}
	if retrieved.Type != "camera" {
		t.Errorf("Type: got %q, want %q", retrieved.Type, "camera")
	}
}

func TestContainsMotionEvent(t *testing.T) {
	if !containsMotionEvent([]string{"VideoMotion", "VideoLoss"}) {
		t.Error("expected true for codes containing VideoMotion")
	}
	if containsMotionEvent([]string{"VideoLoss"}) {
		t.Error("expected false for codes without VideoMotion")
	}
	if containsMotionEvent(nil) {
		t.Error("expected false for nil codes")
	}
}

func TestAmcrestClientNew(t *testing.T) {
	client := NewAmcrestClient("http://192.168.1.100", "admin", "password", 10*time.Second)
	if client.BaseURL != "http://192.168.1.100" {
		t.Errorf("BaseURL: got %q, want %q", client.BaseURL, "http://192.168.1.100")
	}
	if client.Username != "admin" {
		t.Errorf("Username: got %q, want %q", client.Username, "admin")
	}
	if client.Password != "password" {
		t.Errorf("Password: got %q, want %q", client.Password, "password")
	}
}

func TestParseKV(t *testing.T) {
	in := strings.NewReader("a=1\nb=two\n")
	got := parseKV(in)
	if got["a"] != "1" || got["b"] != "two" {
		t.Fatalf("unexpected kv: %+v", got)
	}
}
