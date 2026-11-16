package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// The host stores request router flags and settings from the executable's own
// `manifest` output, which is what a release ships as manifest.json. Without
// apiKeyOptional the host never dispatches a connection that has no key, and
// this plugin's connections never have one. Reading the raw JSON, rather than
// decoding it with this SDK, also catches a field the SDK drops on load.
func TestExecutableManifestDeclaresOptionalRouterConnection(t *testing.T) {
	binary := buildExecutable(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "manifest").Output()
	if err != nil {
		t.Fatalf("manifest CLI: %v", err)
	}
	var manifest struct {
		Capabilities []struct {
			ID            string         `json:"id"`
			RequestRouter map[string]any `json:"requestRouter"`
			ConfigSchema  []configSchema `json:"configSchema"`
		} `json:"capabilities"`
		GlobalConfigSchema []configSchema `json:"globalConfigSchema"`
	}
	if err := json.Unmarshal(output, &manifest); err != nil {
		t.Fatalf("decode manifest output: %v\n%s", err, output)
	}

	var ids []string
	for _, capability := range manifest.Capabilities {
		ids = append(ids, capability.ID)
		for _, flag := range []string{"apiKeyOptional", "baseUrlOptional"} {
			if capability.RequestRouter[flag] != true {
				t.Errorf("capability %q requestRouter.%s = %v, want true", capability.ID, flag, capability.RequestRouter[flag])
			}
		}
		if capability.ID == "ebooks" && !slices.Contains(capability.ConfigSchema[0].fieldKeys(t), "preferred_format") {
			t.Error("ebooks connection settings lack preferred_format")
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"audiobooks", "ebooks"}) {
		t.Fatalf("capabilities = %v, want audiobooks and ebooks", ids)
	}

	var connection *configSchema
	for i := range manifest.GlobalConfigSchema {
		if manifest.GlobalConfigSchema[i].Key == "connection" {
			connection = &manifest.GlobalConfigSchema[i]
		}
	}
	if connection == nil {
		t.Fatal("manifest has no connection settings")
	}
	keys := connection.fieldKeys(t)
	for _, key := range []string{"request_api_key", "preferred_format"} {
		if !slices.Contains(keys, key) {
			t.Errorf("connection settings %v lack %s", keys, key)
		}
	}
}

type configSchema struct {
	Key        string `json:"key"`
	JSONSchema string `json:"jsonSchema"`
	AdminForm  struct {
		Fields []struct {
			Key string `json:"key"`
		} `json:"fields"`
	} `json:"adminForm"`
}

// fieldKeys returns the keys present both in the JSON Schema and on the admin
// form, so a setting the form shows is one the schema accepts.
func (s configSchema) fieldKeys(t *testing.T) []string {
	t.Helper()
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(s.JSONSchema), &schema); err != nil {
		t.Fatalf("%s json schema: %v", s.Key, err)
	}
	var keys []string
	for _, field := range s.AdminForm.Fields {
		if _, ok := schema.Properties[field.Key]; ok {
			keys = append(keys, field.Key)
		}
	}
	return keys
}
