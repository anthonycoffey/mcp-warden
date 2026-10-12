package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// policyFile is the optional <state-dir>/config.json. Everything in it
// extends the built-in defaults; nothing replaces them.
type policyFile struct {
	Match   []string `json:"match"`
	Exclude []string `json:"exclude"`
	Clients []struct {
		Label   string `json:"label"`
		Name    string `json:"name"`
		Cmdline string `json:"cmdline"`
	} `json:"clients"`
}

// ConfigPath is where the optional policy file lives.
func ConfigPath(dir string) string { return filepath.Join(dir, "config.json") }

// loadPolicy merges the defaults, the config file, and command-line patterns.
func loadPolicy(dir string, match, exclude []string) (classify.Config, error) {
	cfg := classify.DefaultConfig()
	var pf policyFile
	if err := state.ReadFile(ConfigPath(dir), &pf); err != nil && !os.IsNotExist(err) {
		return cfg, fmt.Errorf("read %s: %w", ConfigPath(dir), err)
	}
	add := func(dst *[]*regexp.Regexp, patterns []string, what string) error {
		for _, p := range patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("%s pattern %q: %w", what, p, err)
			}
			*dst = append(*dst, re)
		}
		return nil
	}
	if err := add(&cfg.Match, append(pf.Match, match...), "match"); err != nil {
		return cfg, err
	}
	if err := add(&cfg.Exclude, append(pf.Exclude, exclude...), "exclude"); err != nil {
		return cfg, err
	}
	for _, c := range pf.Clients {
		rule := classify.ClientRule{Label: c.Label}
		var err error
		if c.Name != "" {
			if rule.Name, err = regexp.Compile(c.Name); err != nil {
				return cfg, fmt.Errorf("client %q name pattern: %w", c.Label, err)
			}
		}
		if c.Cmdline != "" {
			if rule.Cmdline, err = regexp.Compile(c.Cmdline); err != nil {
				return cfg, fmt.Errorf("client %q cmdline pattern: %w", c.Label, err)
			}
		}
		cfg.Clients = append(cfg.Clients, rule)
	}
	return cfg, nil
}
