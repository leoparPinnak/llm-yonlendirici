package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// config is everything the gateway needs to run. Every field carries a
// default measured against the live Zen service, so an empty config file is
// a valid deployment.
type config struct {
	Listen        string   `json:"listen"`
	Upstream      string   `json:"upstream"`
	DefaultModel  string   `json:"defaultModel"`
	Models        []string `json:"models"`
	UserAgent     string   `json:"userAgent"`
	MinGateTools  int      `json:"minGateTools"`
	OpencodeTools []string `json:"opencodeTools"`
	// ShellTools names the command tools the free-tier gate demands: a
	// tools array without one of them is rejected even when it carries six
	// real opencode names. The first entry is the reserved stand-in the
	// gateway appends when the client sends none.
	ShellTools []string `json:"shellTools"`
	// Retry403 is how many extra attempts follow an upstream 403. The free
	// tier rejects intermittently although the costume is fine; a retry
	// with a fresh session id usually passes.
	Retry403 int `json:"retry403"`
	// UsageLogPath is the JSONL file every exchange is appended to and the
	// /ui statistics read. "-" disables the statistics.
	UsageLogPath string `json:"usageLogPath"`
}

// defaultConfig returns the built-in configuration: the header costume, the
// session id format, and the tool names the free-tier gate was measured to
// require (2026-09-23).
func defaultConfig() config {
	return config{
		Listen:       "127.0.0.1:8110",
		Upstream:     "https://opencode.ai/zen/v1",
		DefaultModel: "mimo-v2.6-flash-free",
		Models:       []string{"mimo-v2.6-flash-free", "space-bunny-free"},
		UserAgent:    "opencode/latest/2.0.14/cli",
		MinGateTools: 6,
		OpencodeTools: []string{
			"edit", "glob", "grep", "question", "read", "shell",
			"skill", "subagent", "webfetch", "websearch", "write", "execute",
		},
		ShellTools:   []string{"shell", "bash"},
		Retry403:     2,
		UsageLogPath: "zen-kostum-kullanim.jsonl",
	}
}

// loadConfig reads path over the defaults, when path is not empty.
func loadConfig(path string) (config, error) {
	cfg := defaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading the config: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parsing the config: %w", err)
		}
	}
	cfg.fillDefaults()
	return cfg, cfg.validate()
}

// fillDefaults replaces empty fields with their built-in values.
func (c *config) fillDefaults() {
	d := defaultConfig()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.Upstream == "" {
		c.Upstream = d.Upstream
	}
	if c.DefaultModel == "" {
		c.DefaultModel = d.DefaultModel
	}
	if len(c.Models) == 0 {
		c.Models = d.Models
	}
	if c.UserAgent == "" {
		c.UserAgent = d.UserAgent
	}
	if c.MinGateTools == 0 {
		c.MinGateTools = d.MinGateTools
	}
	if len(c.OpencodeTools) == 0 {
		c.OpencodeTools = d.OpencodeTools
	}
	if len(c.ShellTools) == 0 {
		c.ShellTools = d.ShellTools
	}
	if c.Retry403 < 0 {
		c.Retry403 = 0
	}
	if c.UsageLogPath == "" {
		c.UsageLogPath = d.UsageLogPath
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
}

// applyModelOverride points the default model at id and replaces it in the
// model list when the list still carries the old default.
func (c *config) applyModelOverride(id string) {
	old := c.DefaultModel
	c.DefaultModel = id
	for i, listed := range c.Models {
		if listed == old {
			c.Models[i] = id
			return
		}
	}
}

// validate reports whether the configuration can serve requests.
func (c config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen is empty")
	}
	parsed, err := url.Parse(c.Upstream)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("upstream %q is not an absolute URL", c.Upstream)
	}
	if c.DefaultModel == "" {
		return fmt.Errorf("defaultModel is empty")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("models is empty")
	}
	if c.UserAgent == "" {
		return fmt.Errorf("userAgent is empty")
	}
	if c.MinGateTools < 1 {
		return fmt.Errorf("minGateTools must be at least 1")
	}
	if c.MinGateTools > len(c.OpencodeTools) {
		return fmt.Errorf("minGateTools (%d) exceeds the %d opencode tool names",
			c.MinGateTools, len(c.OpencodeTools))
	}
	seen := make(map[string]bool, len(c.OpencodeTools))
	for _, name := range c.OpencodeTools {
		if name == "" {
			return fmt.Errorf("opencodeTools contains an empty name")
		}
		if seen[name] {
			return fmt.Errorf("opencodeTools lists %q twice", name)
		}
		seen[name] = true
	}
	if len(c.ShellTools) == 0 {
		return fmt.Errorf("shellTools is empty")
	}
	seenShell := make(map[string]bool, len(c.ShellTools))
	for _, name := range c.ShellTools {
		if name == "" {
			return fmt.Errorf("shellTools contains an empty name")
		}
		if seenShell[name] {
			return fmt.Errorf("shellTools lists %q twice", name)
		}
		seenShell[name] = true
	}
	return nil
}
