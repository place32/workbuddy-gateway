package main

import (
	"sort"
	"strings"
	"sync"
)

// runtimeIdentityConfig controls the display names used when normalizing
// system/developer/assistant text. An absent config replaces known harness
// names with "WorkBuddy".
type runtimeIdentityConfig struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Target       string            `json:"target,omitempty"`
	Replacements map[string]string `json:"replacements,omitempty"`
}

type identityReplacement struct {
	Old string
	New string
}

type identitySettings struct {
	enabled      bool
	prefix       string
	replacements []identityReplacement
}

var (
	defaultIdentityTerms = []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode"}
	identityConfigMu     sync.RWMutex
	currentIdentity      = defaultIdentitySettings()
)

func defaultIdentitySettings() identitySettings {
	target := "WorkBuddy"
	replacements := make([]identityReplacement, 0, len(defaultIdentityTerms))
	for _, term := range defaultIdentityTerms {
		replacements = append(replacements, identityReplacement{Old: term, New: target})
	}
	return identitySettings{enabled: true, prefix: "You are " + target, replacements: replacements}
}

func defaultIdentityConfig() runtimeIdentityConfig {
	enabled := true
	replacements := make(map[string]string, len(defaultIdentityTerms))
	for _, term := range defaultIdentityTerms {
		replacements[term] = "WorkBuddy"
	}
	return runtimeIdentityConfig{Enabled: &enabled, Target: "WorkBuddy", Replacements: replacements}
}

func setIdentityConfig(c runtimeIdentityConfig) {
	enabled := true
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	target := strings.TrimSpace(c.Target)
	if target == "" {
		target = "WorkBuddy"
	}
	prefixTarget := target
	replacements := make([]identityReplacement, 0, len(c.Replacements))
	if len(c.Replacements) > 0 {
		for old, newValue := range c.Replacements {
			if strings.TrimSpace(old) == "" {
				continue
			}
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
		if replacement, ok := c.Replacements["DeepSeek Harness"]; ok && strings.TrimSpace(replacement) != "" {
			prefixTarget = replacement
		}
	} else {
		for _, term := range defaultIdentityTerms {
			replacements = append(replacements, identityReplacement{Old: term, New: target})
		}
	}
	// Longest first avoids splitting "Claude Code" into "Assistant Code".
	sort.SliceStable(replacements, func(i, j int) bool {
		if len(replacements[i].Old) != len(replacements[j].Old) {
			return len(replacements[i].Old) > len(replacements[j].Old)
		}
		return replacements[i].Old < replacements[j].Old
	})
	identityConfigMu.Lock()
	currentIdentity = identitySettings{
		enabled: enabled, prefix: "You are " + prefixTarget, replacements: replacements,
	}
	identityConfigMu.Unlock()
}

func identitySnapshot() identitySettings {
	identityConfigMu.RLock()
	snapshot := currentIdentity
	snapshot.replacements = append([]identityReplacement(nil), currentIdentity.replacements...)
	identityConfigMu.RUnlock()
	return snapshot
}

func identitySettingsSummary() (bool, string, int) {
	settings := identitySnapshot()
	target := strings.TrimPrefix(settings.prefix, "You are ")
	return settings.enabled, target, len(settings.replacements)
}

// normalizeAssistantIdentityText applies the configurable prefix and
// display-name replacements. It is called only for system/developer/assistant
// text; user/tool content is intentionally outside this function.
func normalizeAssistantIdentityText(text string) (string, bool) {
	settings := identitySnapshot()
	if !settings.enabled {
		return text, false
	}
	original := text
	for _, prefix := range blockedSystemPromptPrefixes {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			text = settings.prefix + text[len(prefix):]
			break
		}
	}
	for _, replacement := range settings.replacements {
		text = strings.ReplaceAll(text, replacement.Old, replacement.New)
	}
	return text, text != original
}
