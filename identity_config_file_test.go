package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRuntimeConfigFileCreatesCompleteDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Enabled == nil || !*c.Identity.Enabled || c.Identity.Target != "WorkBuddy" {
		t.Fatalf("default identity=%#v", c.Identity)
	}
	for _, term := range []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode"} {
		if c.Identity.Replacements[term] != "WorkBuddy" {
			t.Fatalf("default replacement %q=%q", term, c.Identity.Replacements[term])
		}
	}
}

func TestEnsureRuntimeConfigFileMigratesOldConfigWithoutIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"gateway":{"apiPort":18317},"systemPrompt":{"force":"keep"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["identity"]; !ok {
		t.Fatal("identity was not added to old config")
	}
	var c runtimeFileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.Identity.Target != "WorkBuddy" || c.Gateway.APIPort != 18317 || c.SystemPrompt.Force != "keep" {
		t.Fatalf("old config was not preserved: %#v", c)
	}
}

func TestEnsureRuntimeConfigFilePreservesExistingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"identity":{"enabled":true,"target":"Assistant"}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Fatalf("existing identity must not be overwritten: %s", raw)
	}
}

func TestEnsureRuntimeConfigFileLeavesInvalidDocumentForValidation(t *testing.T) {
	for _, original := range []string{"null", "[]", "{", `{ "gateway": {} } trailing`} {
		t.Run(original, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ensureRuntimeConfigFile(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != original {
				t.Fatalf("invalid config was changed: %q err=%v", raw, err)
			}
			if _, err := parseRuntimeConfig(raw); err == nil {
				t.Fatal("invalid config must be rejected by normal validation")
			}
		})
	}
}

func TestIdentityConfigSupportsArbitraryReplacementKeys(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	enabled := true
	setIdentityConfig(runtimeIdentityConfig{Enabled: &enabled, Target: "WorkBuddy", Replacements: map[string]string{
		"kimi": "WorkBuddy",
	}})
	got, changed := normalizeAssistantIdentityText("kimi is the assistant")
	if !changed || got != "WorkBuddy is the assistant" {
		t.Fatalf("arbitrary replacement got=%q changed=%t", got, changed)
	}
}

func TestIdentityConfigHotReloadsFromDisk(t *testing.T) {
	isolatedGatewayConfig(t)
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	write := func(target string) {
		data := []byte(`{"gateway":{"apiPort":18317,"webPort":18316},"identity":{"enabled":true,"target":"` + target + `"}}`)
		if err := os.WriteFile(runtimeConfigFile, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Assistant")
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if got, _ := normalizeAssistantIdentityText("You are Codex"); got != "You are Assistant" {
		t.Fatalf("first load got=%q", got)
	}
	write("WorkBuddy")
	if err := loadRuntimeConfig(runtimeConfigFile); err != nil {
		t.Fatal(err)
	}
	if got, _ := normalizeAssistantIdentityText("You are Codex"); got != "You are WorkBuddy" {
		t.Fatalf("hot reload got=%q", got)
	}
}

func TestIdentityConfigEmptyValuesUseDefaultWithoutError(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Target: "", Replacements: map[string]string{}})
	got, changed := normalizeAssistantIdentityText("You are Codex")
	if !changed || got != "You are WorkBuddy" {
		t.Fatalf("empty identity config got=%q changed=%t", got, changed)
	}
}
