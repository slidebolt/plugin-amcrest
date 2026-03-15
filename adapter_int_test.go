//go:build integration

package main

import (
	"testing"

	"github.com/slidebolt/sdk-integration-testing"
)

const pluginIDIntegration = "plugin-amcrest"

func TestIntegration_PluginRegisters(t *testing.T) {
	s := integrationtesting.New(t, "github.com/slidebolt/plugin-amcrest", ".")
	s.RequirePlugin(pluginIDIntegration)

	plugins, err := s.Plugins()
	if err != nil {
		t.Fatalf("GET /api/plugins: %v", err)
	}
	reg, ok := plugins[pluginIDIntegration]
	if !ok {
		t.Fatalf("plugin %q not in registry", pluginIDIntegration)
	}
	t.Logf("registered: id=%s name=%s version=%s", pluginIDIntegration, reg.Manifest.Name, reg.Manifest.Version)
}

func TestIntegration_GatewayHealthy(t *testing.T) {
	s := integrationtesting.New(t, "github.com/slidebolt/plugin-amcrest", ".")
	s.RequirePlugin(pluginIDIntegration)

	var body map[string]any
	if err := s.GetJSON("/_internal/health", &body); err != nil {
		t.Fatalf("health check: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", body["status"])
	}
}
