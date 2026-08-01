package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const PolicyVersion = 1

type PolicyRule struct {
	ID          string   `yaml:"id" json:"id"`
	Kind        string   `yaml:"kind" json:"kind"`
	SourceKind  string   `yaml:"source_kind,omitempty" json:"source_kind,omitempty"`
	Agent       string   `yaml:"agent,omitempty" json:"agent,omitempty"`
	Host        string   `yaml:"host,omitempty" json:"host,omitempty"`
	Contains    []string `yaml:"contains,omitempty" json:"contains,omitempty"`
	SeenKind    string   `yaml:"seen_kind,omitempty" json:"seen_kind,omitempty"`
	PendingKind string   `yaml:"pending_kind,omitempty" json:"pending_kind,omitempty"`
	Action      string   `yaml:"action" json:"action"` // reply|ack
	Reply       string   `yaml:"reply,omitempty" json:"reply,omitempty"`
	Disabled    bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

type PolicyConfig struct {
	Version int          `yaml:"version" json:"version"`
	Rules   []PolicyRule `yaml:"rules" json:"rules"`
}

type PolicyContext struct {
	Kind         string
	SourceKind   string
	Agent        string
	Host         string
	Text         string
	Command      string
	SeenKinds    map[string]bool
	PendingKinds map[string]bool
}

type PolicyDecision struct {
	Matched bool   `json:"matched"`
	RuleID  string `json:"rule_id,omitempty"`
	Action  string `json:"action,omitempty"`
	Reply   string `json:"reply,omitempty"`
	Builtin bool   `json:"builtin,omitempty"`
}

type PolicyService struct {
	Path string
}

func (p *PolicyService) path() string {
	if p != nil && strings.TrimSpace(p.Path) != "" {
		return p.Path
	}
	return PolicyPath()
}

func (p *PolicyService) Load() (*PolicyConfig, error) {
	raw, err := os.ReadFile(p.path())
	if os.IsNotExist(err) {
		return &PolicyConfig{Version: PolicyVersion, Rules: []PolicyRule{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg PolicyConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	if cfg.Version == 0 {
		cfg.Version = PolicyVersion
	}
	if cfg.Version != PolicyVersion {
		return nil, fmt.Errorf("unsupported policy version %d", cfg.Version)
	}
	for i := range cfg.Rules {
		if err := validatePolicyRule(cfg.Rules[i]); err != nil {
			return nil, fmt.Errorf("policy rule %q: %w", cfg.Rules[i].ID, err)
		}
	}
	if cfg.Rules == nil {
		cfg.Rules = []PolicyRule{}
	}
	return &cfg, nil
}

func (p *PolicyService) Save(cfg *PolicyConfig) error {
	if cfg == nil {
		return fmt.Errorf("policy config required")
	}
	cfg.Version = PolicyVersion
	for _, rule := range cfg.Rules {
		if err := validatePolicyRule(rule); err != nil {
			return fmt.Errorf("policy rule %q: %w", rule.ID, err)
		}
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := p.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".policy-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func validatePolicyRule(rule PolicyRule) error {
	if rule.ID == "" || sanitizeID(rule.ID) != rule.ID {
		return fmt.Errorf("id must use letters, digits, dot, dash, or underscore")
	}
	if rule.Kind == "" {
		return fmt.Errorf("kind required")
	}
	switch rule.Action {
	case "reply":
		if rule.Kind != "ask" && rule.Kind != "permission_required" {
			return fmt.Errorf("reply is allowed only for ask or permission_required")
		}
		if strings.TrimSpace(rule.Reply) == "" || len(rule.Reply) > 160 {
			return fmt.Errorf("reply must be 1..160 bytes")
		}
	case "ack":
		if rule.Kind == "ask" || rule.Kind == "permission_required" {
			return fmt.Errorf("input requests cannot be silently acknowledged")
		}
		if rule.Reply != "" {
			return fmt.Errorf("ack rule cannot include reply")
		}
	default:
		return fmt.Errorf("action must be reply or ack")
	}
	if rule.Action == "reply" && len(rule.Contains) == 0 {
		return fmt.Errorf("reply rule requires at least one literal --contains guard")
	}
	for _, needle := range rule.Contains {
		if strings.TrimSpace(needle) == "" {
			return fmt.Errorf("contains values cannot be empty")
		}
	}
	return nil
}

func (p *PolicyService) Add(rule PolicyRule) error {
	if err := validatePolicyRule(rule); err != nil {
		return err
	}
	cfg, err := p.Load()
	if err != nil {
		return err
	}
	for _, existing := range cfg.Rules {
		if existing.ID == rule.ID {
			return fmt.Errorf("policy rule %q already exists", rule.ID)
		}
	}
	cfg.Rules = append(cfg.Rules, rule)
	return p.Save(cfg)
}

func (p *PolicyService) Remove(id string) error {
	cfg, err := p.Load()
	if err != nil {
		return err
	}
	kept := make([]PolicyRule, 0, len(cfg.Rules))
	found := false
	for _, rule := range cfg.Rules {
		if rule.ID == id {
			found = true
			continue
		}
		kept = append(kept, rule)
	}
	if !found {
		return fmt.Errorf("policy rule %q not found", id)
	}
	cfg.Rules = kept
	return p.Save(cfg)
}

func containsKind(values map[string]bool, kind string) bool {
	return kind != "" && values != nil && values[kind]
}

func (p *PolicyService) Decide(ctx PolicyContext) (PolicyDecision, error) {
	// Conservative built-ins remove only redundant receipts. They never grant
	// permission or suppress the sole actionable message.
	if ctx.Kind == "ask" && ctx.SourceKind == "idle" && containsKind(ctx.PendingKinds, "permission_required") {
		return PolicyDecision{Matched: true, RuleID: "builtin.coalesce_permission_idle", Action: "ack", Builtin: true}, nil
	}
	if ctx.SourceKind == "idle" && (ctx.Kind == "ask" || ctx.Kind == "permission_required") && containsKind(ctx.PendingKinds, ctx.Kind) {
		return PolicyDecision{Matched: true, RuleID: "builtin.coalesce_repeated_idle", Action: "ack", Builtin: true}, nil
	}
	if ctx.Kind == "exit" && containsKind(ctx.SeenKinds, "result") {
		return PolicyDecision{Matched: true, RuleID: "builtin.coalesce_result_exit", Action: "ack", Builtin: true}, nil
	}
	cfg, err := p.Load()
	if err != nil {
		return PolicyDecision{}, err
	}
	haystack := strings.ToLower(ctx.Text + "\n" + ctx.Command)
	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.Kind != ctx.Kind || (rule.SourceKind != "" && rule.SourceKind != ctx.SourceKind) || (rule.Agent != "" && rule.Agent != ctx.Agent) || (rule.Host != "" && rule.Host != ctx.Host) {
			continue
		}
		if rule.SeenKind != "" && !containsKind(ctx.SeenKinds, rule.SeenKind) {
			continue
		}
		if rule.PendingKind != "" && !containsKind(ctx.PendingKinds, rule.PendingKind) {
			continue
		}
		matched := true
		for _, needle := range rule.Contains {
			if !strings.Contains(haystack, strings.ToLower(needle)) {
				matched = false
				break
			}
		}
		if matched {
			return PolicyDecision{Matched: true, RuleID: rule.ID, Action: rule.Action, Reply: rule.Reply}, nil
		}
	}
	return PolicyDecision{}, nil
}

func (p *PolicyService) Describe() (path string, builtins []PolicyRule, cfg *PolicyConfig, err error) {
	cfg, err = p.Load()
	if err != nil {
		return p.path(), nil, nil, err
	}
	builtins = []PolicyRule{
		{ID: "builtin.coalesce_permission_idle", Kind: "ask", SourceKind: "idle", PendingKind: "permission_required", Action: "ack"},
		{ID: "builtin.coalesce_repeated_idle", Kind: "ask|permission_required", SourceKind: "idle", PendingKind: "same-kind", Action: "ack"},
		{ID: "builtin.coalesce_result_exit", Kind: "exit", SeenKind: "result", Action: "ack"},
	}
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].ID < builtins[j].ID })
	return p.path(), builtins, cfg, nil
}
