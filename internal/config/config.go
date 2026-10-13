// Package config manages grizzle.yaml and grizzle.json configuration files.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LintConfig configures schema lint behavior.
type LintConfig struct {
	FailOnWarning bool     `json:"fail_on_warning"`
	DisabledRules []string `json:"disabled_rules"`
}

// Config represents project-level configuration for Grizzle.
type Config struct {
	DSN           string     `json:"dsn"`
	Schema        string     `json:"schema"`
	Dialect       string     `json:"dialect"`
	TargetSchema  string     `json:"target_schema"`
	AllowDrop     bool       `json:"allow_drop"`
	Roles         string     `json:"roles"`
	Catalog       string     `json:"catalog"`
	Seed          string     `json:"seed"`
	ShadowSchema  string     `json:"shadow_schema"`
	AcceptHazards []string   `json:"accept_hazards"`
	BeforeSync    string     `json:"before_sync"`
	AfterSync     string     `json:"after_sync"`
	Lint          LintConfig `json:"lint"`
}

// FindConfigFile discovers a configuration file in the working directory or uses explicit path.
func FindConfigFile(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := []string{
		"grizzle.yaml",
		".grizzle.yaml",
		"grizzle.yml",
		".grizzle.yml",
		"grizzle.json",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// Load reads and parses a configuration file from path.
func Load(path string) (*Config, error) {
	if path == "" {
		return &Config{}, nil
	}

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	cfg := &Config{}

	// If valid JSON, unmarshal directly
	if json.Valid(data) {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing json config %q: %w", path, err)
		}
		return cfg, nil
	}

	// Parse YAML line by line
	if err := parseYAML(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parsing yaml config %q: %w", path, err)
	}

	return cfg, nil
}

func parseYAML(content string, cfg *Config) error {
	scanner := bufio.NewScanner(strings.NewReader(content))
	currentSection := ""
	currentListKey := ""

	for scanner.Scan() {
		rawLine := scanner.Text()
		line := strings.TrimRight(rawLine, " \t\r")

		// Strip comments
		if idx := strings.Index(line, "#"); idx != 0 && idx > 0 {
			line = line[:idx]
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Check for list item "- value"
		if strings.HasPrefix(trimmed, "-") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			val = strings.Trim(val, `"'`)
			switch currentListKey {
			case "accept_hazards":
				cfg.AcceptHazards = append(cfg.AcceptHazards, val)
			case "disabled_rules":
				cfg.Lint.DisabledRules = append(cfg.Lint.DisabledRules, val)
			}
			continue
		}

		// Check for section header or key: value
		k, v, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)

		// Indented under a section?
		isIndented := len(rawLine) > len(strings.TrimLeft(rawLine, " \t"))
		if !isIndented {
			currentSection = ""
			currentListKey = ""
		}

		if v == "" {
			// Key starts a list or a nested section
			if k == "lint" {
				currentSection = "lint"
			} else if k == "accept_hazards" {
				currentListKey = "accept_hazards"
			} else if k == "disabled_rules" && currentSection == "lint" {
				currentListKey = "disabled_rules"
			}
			continue
		}

		if currentSection == "lint" {
			if k == "fail_on_warning" {
				b, _ := strconv.ParseBool(v)
				cfg.Lint.FailOnWarning = b
			}
			continue
		}

		switch k {
		case "dsn":
			cfg.DSN = v
		case "schema":
			cfg.Schema = v
		case "dialect":
			cfg.Dialect = v
		case "target_schema":
			cfg.TargetSchema = v
		case "allow_drop":
			b, _ := strconv.ParseBool(v)
			cfg.AllowDrop = b
		case "roles":
			cfg.Roles = v
		case "catalog":
			cfg.Catalog = v
		case "seed":
			cfg.Seed = v
		case "shadow_schema":
			cfg.ShadowSchema = v
		case "before_sync":
			cfg.BeforeSync = v
		case "after_sync":
			cfg.AfterSync = v
		}
	}

	return scanner.Err()
}
