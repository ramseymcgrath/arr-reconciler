package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Instance describes a single Sonarr/Radarr endpoint.
type Instance struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "sonarr" or "radarr"
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// Safety holds the hard guardrails that LLM decisions cannot override.
type Safety struct {
	// TrashDataset is a ZFS dataset (mounted path) where "deleted" files are
	// moved instead of being unlinked. Required when DeleteEnabled is true.
	TrashDataset string `json:"trash_dataset"`
	// TrashTTL is how long files survive in the trash before purge.
	TrashTTL Duration `json:"trash_ttl"`
	// DeleteEnabled toggles destructive orphan handling. When false, orphans
	// are only reported.
	DeleteEnabled bool `json:"delete_enabled"`
	// MaxDeletesPerRun caps how many orphan files a single run may relocate.
	MaxDeletesPerRun int `json:"max_deletes_per_run"`
	// MaxBytesPerRun caps total bytes relocated in a single run.
	MaxBytesPerRun int64 `json:"max_bytes_per_run"`
	// AllowedRoots restricts orphan handling to files under these paths.
	// A file must be under one of these prefixes or it is never touched.
	AllowedRoots []string `json:"allowed_roots"`
	// ProtectedExtensions are never relocated regardless of LLM decision
	// (e.g. ".nfo", ".srt" you maintain by hand). Lowercase, with dot.
	ProtectedExtensions []string `json:"protected_extensions"`
	// MinOrphanAge requires a file's mtime to be older than this before it is
	// eligible for relocation, avoiding races with in-flight imports.
	MinOrphanAge Duration `json:"min_orphan_age"`
	// MaxCandidatesPerRun caps how many orphan candidates are sent to the model
	// in a single run, sliced into batches of BatchSize. This bounds token cost
	// and avoids exceeding the model's context window on a large library. The
	// rest are simply deferred to the next run. Defaults to 500.
	MaxCandidatesPerRun int `json:"max_candidates_per_run"`
	// BatchSize is how many candidates are sent per model request. Defaults to
	// 50. Smaller batches keep each request well inside the context window and
	// make a single bad response cost less.
	BatchSize int `json:"batch_size"`
	// RulesDisabled turns OFF tier-0 deterministic rules that auto-trash narrow,
	// unambiguous junk (zero-byte/orphaned sidecars, sample clips) without a
	// model. Rules are still subject to every hard rail before any move. They are
	// ON by default (zero value); set this true to route everything through the
	// models instead.
	RulesDisabled bool `json:"rules_disabled"`
}

// Queue holds stuck-queue-item handling parameters.
type Queue struct {
	// StalledThreshold is how long an item may sit without progress before it
	// is considered stuck.
	StalledThreshold Duration `json:"stalled_threshold"`
	// MaxRemovalsPerRun caps queue removals in a single run.
	MaxRemovalsPerRun int `json:"max_removals_per_run"`
	// Blocklist controls whether removed items are added to the arr blocklist
	// so the same release is not re-grabbed.
	Blocklist bool `json:"blocklist"`
	// MaxCandidatesPerRun caps how many stuck queue items are triaged per run,
	// sliced into BatchSize requests. Bounds token cost / context size on a huge
	// queue; overflow defers to the next run. Defaults to 200.
	MaxCandidatesPerRun int `json:"max_candidates_per_run"`
	// BatchSize is how many queue items are sent per model request. Defaults to
	// 50.
	BatchSize int `json:"batch_size"`
	// GraceRuns requires an item to look stuck for this many CONSECUTIVE runs
	// before it is eligible for removal, so a transiently stalled download that
	// recovers on its own is never removed. 1 means act on first sighting (no
	// grace). Defaults to 2.
	GraceRuns int `json:"grace_runs"`
}

// Claude holds Anthropic API configuration.
type Claude struct {
	APIKey string `json:"api_key"`
	// Model is the default/fallback model used when a per-task model is unset.
	Model string `json:"model"`
	// QueueModel overrides Model for download-queue triage. That task is
	// reversible (removal just re-searches) and the signals are simple, so a
	// small fast model is appropriate. Defaults to a Haiku-class model.
	QueueModel string `json:"queue_model"`
	// OrphanModel overrides Model for orphan-file triage. That task is the
	// destructive path and demands nuanced judgement (junk vs. hand-managed
	// sidecar) on a large pool, so it uses a stronger model than the queue.
	// Defaults to Model.
	OrphanModel string `json:"orphan_model"`
	MaxTokens   int    `json:"max_tokens"`
	// BaseURL is the Messages API base ("/v1/messages" is appended). Point it at
	// a Cloudflare AI Gateway Anthropic-native path to route through the gateway.
	BaseURL string `json:"base_url"`
	// GatewayToken, when set, authenticates to a Cloudflare AI Gateway via the
	// cf-aig-authorization header. Required if BaseURL is an authenticated
	// gateway endpoint; ignored when going direct to Anthropic.
	GatewayToken string `json:"gateway_token"`
	// CacheTTL is the Cloudflare AI Gateway response cache TTL. When > 0 it sets
	// cf-aig-cache-ttl so identical requests within the window are served from
	// the gateway cache. Only meaningful with a gateway BaseURL.
	CacheTTL Duration `json:"cache_ttl"`
	// Timeout bounds a single Messages API call. Model calls are far slower than
	// the arr REST calls (HTTPTimeout), so this is separate and longer.
	// Defaults to 2m.
	Timeout Duration `json:"timeout"`
}

// Notify holds webhook notification config (Discord/Slack-compatible).
type Notify struct {
	WebhookURL string `json:"webhook_url"`
}

// Local configures the cheap local-model prefilter tier (Ollama). When Endpoint
// is empty the tier is disabled and every candidate escalates to the frontier
// model unchanged.
type Local struct {
	// Endpoint is the Ollama base URL, e.g. "http://ollama:11434". Empty
	// disables the prefilter.
	Endpoint string `json:"endpoint"`
	// Model is the Ollama model tag, e.g. "qwen2.5:3b".
	Model string `json:"model"`
	// Timeout bounds a single classification call. Defaults to 1m.
	Timeout Duration `json:"timeout"`
}

// Enabled reports whether the local prefilter tier is active.
func (l Local) Enabled() bool { return l.Endpoint != "" }

// LLMObs configures Datadog LLM Observability. Spans and evaluations are POSTed
// to a local Datadog agent's EVP proxy (no API key required in agent mode). It
// is disabled entirely when Endpoint is empty.
type LLMObs struct {
	// Endpoint is the base URL of the Datadog agent's LLM Obs intake, e.g.
	// "http://datadog-agent:8126". Empty disables all instrumentation.
	Endpoint string `json:"endpoint"`
	// MLApp is the Datadog ml_app name (lowercase, no trailing/contiguous
	// underscores). Defaults to "arr-reconciler".
	MLApp string `json:"ml_app"`
	// Tags are extra tags applied to every span (e.g. "env:home").
	Tags []string `json:"tags"`
}

// Enabled reports whether LLM Observability instrumentation should run.
func (l LLMObs) Enabled() bool { return l.Endpoint != "" }

// Config is the full reconciler configuration.
type Config struct {
	Instances   []Instance `json:"instances"`
	Safety      Safety     `json:"safety"`
	Queue       Queue      `json:"queue"`
	Claude      Claude     `json:"claude"`
	Notify      Notify     `json:"notify"`
	LLMObs      LLMObs     `json:"llmobs"`
	Local       Local      `json:"local"`
	DryRun      bool       `json:"dry_run"`
	HTTPTimeout Duration   `json:"http_timeout"`
	StateFile   string     `json:"state_file"`
}

// Duration is a JSON-friendly time.Duration ("15m", "72h", ...).
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads, parses, applies defaults to, and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Claude.Model == "" {
		// Sonnet is the default: strong enough for the destructive orphan path,
		// far cheaper than Opus for a job that runs every few hours.
		c.Claude.Model = "claude-sonnet-4-6"
	}
	if c.Claude.OrphanModel == "" {
		// Orphan triage is destructive and judgement-heavy: use the default
		// (Sonnet-class) model.
		c.Claude.OrphanModel = c.Claude.Model
	}
	if c.Claude.QueueModel == "" {
		// Queue triage is reversible and simple: a small fast model suffices.
		c.Claude.QueueModel = "claude-haiku-4-5"
	}
	if c.Claude.MaxTokens == 0 {
		c.Claude.MaxTokens = 4096
	}
	if c.Claude.BaseURL == "" {
		c.Claude.BaseURL = "https://api.anthropic.com"
	}
	if c.Claude.Timeout == 0 {
		c.Claude.Timeout = Duration(2 * time.Minute)
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = Duration(30 * time.Second)
	}
	if c.Queue.StalledThreshold == 0 {
		c.Queue.StalledThreshold = Duration(2 * time.Hour)
	}
	if c.Queue.MaxRemovalsPerRun == 0 {
		c.Queue.MaxRemovalsPerRun = 25
	}
	if c.Queue.MaxCandidatesPerRun == 0 {
		c.Queue.MaxCandidatesPerRun = 200
	}
	if c.Queue.BatchSize == 0 {
		c.Queue.BatchSize = 50
	}
	if c.Queue.GraceRuns == 0 {
		c.Queue.GraceRuns = 2
	}
	if c.Local.Model == "" {
		c.Local.Model = "qwen2.5:3b"
	}
	if c.Local.Timeout == 0 {
		c.Local.Timeout = Duration(time.Minute)
	}
	if c.Safety.MaxDeletesPerRun == 0 {
		c.Safety.MaxDeletesPerRun = 50
	}
	if c.Safety.MaxBytesPerRun == 0 {
		c.Safety.MaxBytesPerRun = 500 << 30 // 500 GiB
	}
	if c.Safety.MinOrphanAge == 0 {
		c.Safety.MinOrphanAge = Duration(6 * time.Hour)
	}
	if c.Safety.MaxCandidatesPerRun == 0 {
		c.Safety.MaxCandidatesPerRun = 500
	}
	if c.Safety.BatchSize == 0 {
		c.Safety.BatchSize = 50
	}
	if c.Safety.TrashTTL == 0 {
		c.Safety.TrashTTL = Duration(14 * 24 * time.Hour)
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/arr-reconciler/state.json"
	}
	if c.LLMObs.MLApp == "" {
		c.LLMObs.MLApp = "arr-reconciler"
	}
	// Normalize extensions to lowercase with leading dot.
	for i, e := range c.Safety.ProtectedExtensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		c.Safety.ProtectedExtensions[i] = e
	}
	// Clean allowed roots to absolute, slash-trimmed prefixes.
	for i, r := range c.Safety.AllowedRoots {
		c.Safety.AllowedRoots[i] = filepath.Clean(r)
	}
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("no instances configured")
	}
	for i, inst := range c.Instances {
		if inst.Name == "" {
			return fmt.Errorf("instance %d: name required", i)
		}
		if inst.Kind != "sonarr" && inst.Kind != "radarr" {
			return fmt.Errorf("instance %q: kind must be sonarr or radarr, got %q", inst.Name, inst.Kind)
		}
		if inst.BaseURL == "" {
			return fmt.Errorf("instance %q: base_url required", inst.Name)
		}
		if inst.APIKey == "" {
			return fmt.Errorf("instance %q: api_key required", inst.Name)
		}
	}
	if c.Claude.APIKey == "" {
		return fmt.Errorf("claude.api_key required")
	}
	if c.Safety.DeleteEnabled {
		if c.Safety.TrashDataset == "" {
			return fmt.Errorf("safety.trash_dataset required when delete_enabled is true")
		}
		if len(c.Safety.AllowedRoots) == 0 {
			return fmt.Errorf("safety.allowed_roots required when delete_enabled is true")
		}
	}
	return nil
}

// IsProtectedExt reports whether the given path carries a protected extension.
func (s Safety) IsProtectedExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, p := range s.ProtectedExtensions {
		if ext == p {
			return true
		}
	}
	return false
}

// WithinAllowedRoot reports whether path is under one of the allowed roots.
func (s Safety) WithinAllowedRoot(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range s.AllowedRoots {
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
