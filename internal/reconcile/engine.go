package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ramseymcgrath/arr-reconciler/internal/arr"
	"github.com/ramseymcgrath/arr-reconciler/internal/claude"
	"github.com/ramseymcgrath/arr-reconciler/internal/config"
	"github.com/ramseymcgrath/arr-reconciler/internal/llmobs"
	"github.com/ramseymcgrath/arr-reconciler/internal/local"
	"github.com/ramseymcgrath/arr-reconciler/internal/notify"
	"github.com/ramseymcgrath/arr-reconciler/internal/trash"
)

// Engine runs a single reconciliation pass across all instances.
type Engine struct {
	cfg       *config.Config
	clients   []*arr.Client
	brain     *claude.Client
	prefilter *local.Classifier
	bin       *trash.Bin
	note      *notify.Notifier
	tracer    *llmobs.Tracer
	grace     *graceStore
	log       *slog.Logger
}

// New builds an Engine from config.
func New(cfg *config.Config, log *slog.Logger) *Engine {
	clients := make([]*arr.Client, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		clients = append(clients, arr.New(inst, cfg.HTTPTimeout.Std()))
	}
	e := &Engine{
		cfg:     cfg,
		clients: clients,
		brain: claude.New(claude.Options{
			APIKey:          cfg.Claude.APIKey,
			Model:           cfg.Claude.Model,
			MaxTokens:       cfg.Claude.MaxTokens,
			BaseURL:         cfg.Claude.BaseURL,
			GatewayToken:    cfg.Claude.GatewayToken,
			CacheTTLSeconds: int(cfg.Claude.CacheTTL.Std().Seconds()),
			Timeout:         cfg.Claude.Timeout.Std(),
		}),
		prefilter: local.New(cfg.Local.Endpoint, cfg.Local.Model, cfg.Local.Timeout.Std()),
		note:      notify.New(cfg.Notify.WebhookURL, cfg.HTTPTimeout.Std()),
		tracer:    llmobs.New(cfg.LLMObs.Endpoint, cfg.LLMObs.MLApp, cfg.LLMObs.Tags, cfg.HTTPTimeout.Std()),
		log:       log,
	}
	if cfg.Safety.DeleteEnabled {
		e.bin = trash.New(cfg.Safety.TrashDataset, cfg.Safety.TrashTTL.Std())
	}
	return e
}

// Report is the outcome of a run, suitable for logging/notification.
type Report struct {
	Started        time.Time      `json:"started"`
	Finished       time.Time      `json:"finished"`
	DryRun         bool           `json:"dry_run"`
	QueueRemoved   []ActionRecord `json:"queue_removed"`
	OrphansTrashed []ActionRecord `json:"orphans_trashed"`
	MissingFiles   []ActionRecord `json:"missing_files"`
	Skipped        []ActionRecord `json:"skipped"`
	Errors         []string       `json:"errors"`
	TrashPurged    int            `json:"trash_purged"`
	Funnel         FunnelStats    `json:"funnel"`
}

// FunnelStats counts how many candidates each tier handled, so the cost funnel
// can be tuned. RuleJunk + LocalKeep never reach the frontier model; Escalated
// is what Claude actually saw.
type FunnelStats struct {
	Candidates int `json:"candidates"` // total after the hard rails pre-filter
	RuleJunk   int `json:"rule_junk"`  // tier-0: auto-trashed by deterministic rule
	LocalKeep  int `json:"local_keep"` // tier-1: dropped as safe by the local model
	Escalated  int `json:"escalated"`  // tier-2: sent to the frontier model (Claude)
}

// ActionRecord captures a single action (or non-action) taken on a candidate.
type ActionRecord struct {
	Instance string `json:"instance"`
	Item     string `json:"item"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	Bytes    int64  `json:"bytes,omitempty"`
}

// Run performs a full reconciliation pass.
func (e *Engine) Run(ctx context.Context) (*Report, error) {
	rep := &Report{Started: time.Now(), DryRun: e.cfg.DryRun}

	// Open one LLM Observability trace per run (no-op when disabled). Every
	// Claude call below becomes a child span, with per-decision evaluations.
	trace := e.tracer.StartRun("reconcile_run")

	// Load the queue-grace streak store and start this run's accounting. Items
	// not seen this run have their streaks dropped at commit.
	e.grace = loadGraceStore(e.cfg.StateFile)
	e.grace.begin()

	// Warm the local prefilter model so the first batch isn't slowed by a cold
	// load (best-effort; a failure just means the first call pays the load cost).
	if err := e.prefilter.Preload(ctx); err != nil {
		e.log.Warn("local prefilter preload failed", "err", err)
	}

	for _, c := range e.clients {
		if err := e.reconcileQueue(ctx, c, rep, trace); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("queue %s: %v", c.Name(), err))
			e.log.Error("queue reconcile failed", "instance", c.Name(), "err", err)
		}
		if err := e.reconcileFiles(ctx, c, rep, trace); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("files %s: %v", c.Name(), err))
			e.log.Error("file reconcile failed", "instance", c.Name(), "err", err)
		}
	}

	if e.bin != nil {
		n, err := e.bin.Purge()
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("trash purge: %v", err))
		}
		rep.TrashPurged = n
	}

	rep.Finished = time.Now()
	if err := e.grace.commit(); err != nil {
		e.log.Warn("persist queue-grace failed", "err", err)
	}
	if err := trace.Flush(ctx); err != nil {
		e.log.Warn("llmobs flush failed", "err", err)
	}
	if err := e.persist(rep); err != nil {
		e.log.Warn("persist state failed", "err", err)
	}
	if err := e.note.Send(ctx, rep.Summary()); err != nil {
		e.log.Warn("notify failed", "err", err)
	}
	return rep, nil
}

// ---- Queue reconciliation -------------------------------------------------

// queueCandidate is the minimal, stable view of a stuck queue item presented to
// the model. Empty fields are omitted to keep payloads small, and the two
// continuous values (age, percent remaining) are bucketed/rounded so an
// unchanged stuck set serializes identically across runs (gateway-cacheable).
type queueCandidate struct {
	Ref                   string   `json:"ref"`
	Title                 string   `json:"title"`
	Status                string   `json:"status,omitempty"`
	TrackedDownloadStatus string   `json:"tracked_download_status,omitempty"`
	TrackedDownloadState  string   `json:"tracked_download_state,omitempty"`
	ErrorMessage          string   `json:"error_message,omitempty"`
	Messages              []string `json:"messages,omitempty"`
	AgeHours              int64    `json:"age_hours"`         // bucketed, see ageBucket
	PercentRemaining      int      `json:"percent_remaining"` // rounded to whole percent
	Protocol              string   `json:"protocol,omitempty"`
	Indexer               string   `json:"indexer,omitempty"`
}

const queueSystemPrompt = `You are a download-queue triage agent for a Sonarr/Radarr media automation stack.
You are given a JSON array of stuck or problematic queue items. For each item, decide an action.

Allowed actions:
- "remove": the download is dead, failed, or will not complete (e.g. stalled with no peers, qBittorrent reports error, unpack failures, missing files at the client). Removing also blocklists the release so a different one is grabbed.
- "keep": the item is making progress or is a transient state that will resolve itself; do not touch it.
- "skip": insufficient information to decide safely.

Be conservative: only "remove" when the evidence clearly indicates the download will not succeed. A high age alone is not sufficient if it is still downloading. Warnings about importing (e.g. waiting for files to be moved) usually resolve themselves; prefer "keep".

Respond with ONLY a JSON array, no prose, no markdown fences. Each element:
{"ref": "<the ref you were given>", "action": "remove|keep|skip", "reason": "<short>", "confidence": <0..1>}`

func (e *Engine) reconcileQueue(ctx context.Context, c *arr.Client, rep *Report, trace *llmobs.Trace) error {
	records, err := c.Queue(ctx)
	if err != nil {
		return err
	}
	threshold := e.cfg.Queue.StalledThreshold.Std()

	// Identify candidates: items that look stuck AND have looked stuck for at
	// least GraceRuns consecutive runs (so a transient stall that recovers is
	// never removed). Every stuck item bumps its streak; only those at/over the
	// threshold proceed.
	refIndex := make(map[string]arr.QueueRecord)
	var candidates []queueCandidate
	now := time.Now()
	grace := e.cfg.Queue.GraceRuns
	for _, r := range records {
		if !isStuck(r, now, threshold) {
			continue
		}
		streak := e.grace.seen(queueGraceKey(c.Name(), r))
		if streak < grace {
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: r.Title, Action: "queue-grace",
				Reason: fmt.Sprintf("stuck %d/%d consecutive runs; waiting", streak, grace),
			})
			continue
		}
		ref := fmt.Sprintf("q-%d", r.ID)
		refIndex[ref] = r
		var msgs []string
		for _, sm := range r.StatusMessages {
			msgs = append(msgs, sm.Messages...)
		}
		pctRemaining := 0
		if r.Size > 0 {
			pctRemaining = int((r.Sizeleft / r.Size) * 100)
		}
		candidates = append(candidates, queueCandidate{
			Ref:                   ref,
			Title:                 r.Title,
			Status:                r.Status,
			TrackedDownloadStatus: r.TrackedDownloadStatus,
			TrackedDownloadState:  r.TrackedDownloadState,
			ErrorMessage:          r.ErrorMessage,
			Messages:              dedupe(msgs),
			AgeHours:              ageBucket(now.Sub(r.Added)),
			PercentRemaining:      pctRemaining,
			Protocol:              r.Protocol,
			Indexer:               r.Indexer,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	removed := 0
	apply := func(d claude.Decision, dspan, parent *llmobs.Span) {
		r, ok := refIndex[d.Ref]
		if !ok {
			return
		}
		switch d.Action {
		case "remove":
			if removed >= e.cfg.Queue.MaxRemovalsPerRun {
				rep.Skipped = append(rep.Skipped, ActionRecord{
					Instance: c.Name(), Item: r.Title, Action: "queue-remove",
					Reason: "removal cap reached for this run",
				})
				e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, r.Title, d, "capped_removals")
				return
			}
			rec := ActionRecord{Instance: c.Name(), Item: r.Title, Action: "queue-remove", Reason: d.Reason}
			if e.cfg.DryRun {
				rec.Reason = "[dry-run] " + rec.Reason
				rep.QueueRemoved = append(rep.QueueRemoved, rec)
				removed++
				e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, r.Title, d, "dry_run")
				return
			}
			if err := c.DeleteQueueItem(ctx, r.ID, true, e.cfg.Queue.Blocklist); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("delete queue %s/%d: %v", c.Name(), r.ID, err))
				e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, r.Title, d, "error")
				return
			}
			rep.QueueRemoved = append(rep.QueueRemoved, rec)
			removed++
			e.log.Info("removed stuck queue item", "instance", c.Name(), "title", r.Title, "reason", d.Reason)
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, r.Title, d, "executed")
		default:
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: r.Title, Action: "queue-" + d.Action, Reason: d.Reason,
			})
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, r.Title, d, "kept")
		}
	}

	// Tier-1 local prefilter: drop items the local model is confident are fine
	// (still progressing / transient), escalate the rest. No tier-0 rules here —
	// queue removal hinges on free-text status messages, not structural patterns.
	rep.Funnel.Candidates += len(candidates)
	candidates = e.funnelQueue(ctx, rep, candidates)

	runBatched(e, ctx, c, rep, trace, "queue_triage", e.cfg.Claude.QueueModel, queueSystemPrompt,
		e.cfg.Queue.MaxCandidatesPerRun, e.cfg.Queue.BatchSize, candidates, apply)
	return nil
}

func isStuck(r arr.QueueRecord, now time.Time, threshold time.Duration) bool {
	// Explicit warning/error states are always candidates.
	if strings.EqualFold(r.TrackedDownloadStatus, "warning") ||
		strings.EqualFold(r.TrackedDownloadStatus, "error") {
		return true
	}
	if r.ErrorMessage != "" {
		return true
	}
	// Stalled: download client reports stalled/queued with age past threshold.
	stalledStatus := strings.EqualFold(r.Status, "warning") ||
		strings.Contains(strings.ToLower(r.Status), "stall")
	if stalledStatus && now.Sub(r.Added) > threshold {
		return true
	}
	// Not downloading and old.
	if !r.Added.IsZero() && now.Sub(r.Added) > threshold &&
		r.Sizeleft > 0 && r.EstimatedCompletion == nil {
		return true
	}
	return false
}

// ---- Observability helpers ------------------------------------------------

// llmFinish builds the FinishOpts for an "llm" span wrapping a single Claude
// call: input payload, output text, token usage, model, and any error. model is
// the model requested for this call, used even when the call errored before a
// result came back.
func (e *Engine) llmFinish(model, input string, res *claude.Result, callErr error) llmobs.FinishOpts {
	opt := llmobs.FinishOpts{
		Kind:     "llm",
		Input:    input,
		Provider: "anthropic",
		Model:    model,
		Err:      callErr,
	}
	if res != nil {
		opt.Model = res.Model
		opt.InputTokens = res.Usage.InputTokens
		opt.OutputTokens = res.Usage.OutputTokens
		if raw, err := json.Marshal(res.Decisions); err == nil {
			opt.Output = string(raw)
		}
	}
	return opt
}

// recordDecision closes a per-decision "task" span and attaches two evaluations:
// the model's self-reported confidence (score) and the rail outcome — whether
// the hard safety rails let the model's action through ("executed"), vetoed it
// ("capped_removals", "capped_deletes", "capped_bytes", "blocked_recheck",
// "error"), or it was an inherently safe choice ("kept", "dry_run").
func (e *Engine) recordDecision(trace *llmobs.Trace, span, parent *llmobs.Span, name, item string, d claude.Decision, outcome string) {
	trace.Finish(span, parent, name, llmobs.FinishOpts{
		Kind:   "task",
		Input:  item,
		Output: fmt.Sprintf("action=%s outcome=%s: %s", d.Action, outcome, d.Reason),
	})
	conf := d.Confidence
	trace.Eval(span, llmobs.Eval{Label: "model_confidence", Score: &conf, Reasoning: d.Reason})
	trace.Eval(span, llmobs.Eval{Label: "rail_outcome", Categorical: outcome, Reasoning: d.Reason})
}

// ageBucket collapses a duration into a coarse, stable hour value. The model
// only needs the order of magnitude ("hours" vs "days" vs "weeks"), and
// bucketing means a candidate's serialized age does not drift run-to-run, so an
// otherwise-unchanged payload stays byte-identical and the gateway can cache it.
func ageBucket(d time.Duration) int64 {
	h := int64(d.Hours())
	switch {
	case h < 0:
		return 0
	case h <= 24:
		return h // hourly granularity within the first day
	case h <= 24*7:
		return (h / 24) * 24 // daily buckets within the first week
	default:
		return (h / (24 * 7)) * 24 * 7 // weekly buckets thereafter
	}
}

// dedupe returns the input with duplicate and empty strings removed, preserving
// first-seen order. arr status messages frequently repeat the same line.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// runBatched caps a candidate slice to maxPerRun, slices it into batches of
// batchSize, sends each batch to the model as one "<name>" llm span, and invokes
// apply for every decision the model returns (matched back to its candidate by
// ref). It is the single place both the queue and orphan paths share for the
// cap → batch → call → per-decision-handling flow.
//
// A batch whose model call fails is recorded as an error and skipped; later
// batches still run. apply receives the decision, the open task span for that
// decision, and the batch's llm span (its parent).
func runBatched[T any](
	e *Engine, ctx context.Context, c *arr.Client, rep *Report, trace *llmobs.Trace,
	name, model, system string, maxPerRun, batchSize int,
	candidates []T,
	apply func(d claude.Decision, dspan, parent *llmobs.Span),
) {
	if len(candidates) == 0 {
		return
	}
	if maxPerRun > 0 && len(candidates) > maxPerRun {
		e.log.Info("capping candidates for this run",
			"kind", name, "instance", c.Name(), "found", len(candidates), "cap", maxPerRun)
		candidates = candidates[:maxPerRun]
	}
	if batchSize <= 0 {
		batchSize = len(candidates)
	}

	// Split into chunks of batchSize; each chunk is one model request (one llm
	// span). The chunks are dispatched either synchronously (one HTTP call each)
	// or together via the Message Batches API (50% cost, async) per config.
	type chunk struct {
		label   string
		payload string
	}
	var chunks []chunk
	for start := 0; start < len(candidates); start += batchSize {
		end := start + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		payload, err := json.Marshal(candidates[start:end])
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s marshal %s [%d:%d]: %v", name, c.Name(), start, end, err))
			continue
		}
		chunks = append(chunks, chunk{label: fmt.Sprintf("%s[%d:%d]", name, start, end), payload: string(payload)})
	}
	if len(chunks) == 0 {
		return
	}

	dispatch := func(label, payload string, res *claude.Result, callErr error) {
		span := trace.Start()
		trace.Finish(span, trace.Root(), name+":"+c.Name(), e.llmFinish(model, payload, res, callErr))
		if callErr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s %s %s: %v", name, c.Name(), label, callErr))
			e.log.Error("triage batch failed", "kind", name, "instance", c.Name(), "err", callErr)
			return
		}
		for _, d := range res.Decisions {
			apply(d, trace.Start(), span)
		}
	}

	if e.cfg.Claude.BatchMode {
		// One Message Batch for all chunks: 50% cheaper, asynchronous.
		reqs := make([]claude.BatchRequest, len(chunks))
		for i, ch := range chunks {
			reqs[i] = claude.BatchRequest{
				CustomID: fmt.Sprintf("c%d", i),
				Model:    model,
				System:   system,
				Payload:  ch.payload,
			}
		}
		bctx, cancel := context.WithTimeout(ctx, e.cfg.Claude.BatchTimeout.Std())
		defer cancel()
		e.log.Info("submitting message batch", "kind", name, "instance", c.Name(), "requests", len(reqs))
		results, err := e.brain.DecideBatch(bctx, reqs, e.cfg.Claude.BatchPollInterval.Std())
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s batch %s: %v", name, c.Name(), err))
			e.log.Error("message batch failed", "kind", name, "instance", c.Name(), "err", err)
			return
		}
		byID := make(map[string]claude.BatchResult, len(results))
		for _, r := range results {
			byID[r.CustomID] = r
		}
		for i, ch := range chunks {
			br := byID[fmt.Sprintf("c%d", i)]
			dispatch(ch.label, ch.payload, br.Result, br.Err)
		}
		return
	}

	// Synchronous: one HTTP call per chunk.
	for _, ch := range chunks {
		res, err := e.brain.Decide(ctx, model, system, ch.payload)
		dispatch(ch.label, ch.payload, res, err)
	}
}

// ---- File reconciliation (DB vs disk) -------------------------------------

const orphanSystemPrompt = `You are a filesystem reconciliation agent for a Sonarr/Radarr media library.
You are given a JSON array of ORPHAN files: files found on disk under managed library roots that no Sonarr/Radarr database references. For each, decide an action.

Allowed actions:
- "trash": the file is genuinely orphaned junk safe to relocate to a recoverable trash area (e.g. leftover sample files, failed-import remnants, duplicate releases the library no longer tracks, partial files).
- "keep": the file should NOT be touched. Choose this for anything ambiguous, anything that looks like a companion/sidecar to tracked media (subtitles, artwork, nfo), anything under a path suggesting it is intentionally hand-managed, or anything you are unsure about.

Be extremely conservative. This operates on a 327TB media pool. When in doubt, "keep". Relocation goes to a TTL trash, but minimizing false positives matters far more than reclaiming space.

Respond with ONLY a JSON array, no prose, no markdown fences. Each element:
{"ref": "<the ref you were given>", "action": "trash|keep", "reason": "<short>", "confidence": <0..1>}`

// orphanCandidate is the minimal, stable view of an on-disk file with no DB
// reference that we present to the model. Fields are kept to what actually
// informs a trash/keep judgement (path tells it the name + location; ext, size
// bucket, and age bucket give the rest). Continuous values are bucketed so an
// unchanged set of orphans serializes to a byte-identical payload across runs,
// which lets the Cloudflare AI Gateway cache hit instead of drifting every run.
type orphanCandidate struct {
	Ref      string `json:"ref"`
	Path     string `json:"path"`
	Ext      string `json:"ext"`
	SizeMB   int64  `json:"size_mb"`   // truncated to whole MB
	AgeHours int64  `json:"age_hours"` // bucketed, see ageBucket
}

// fileOnDisk pairs an absolute path with the library root it belongs under.
type fileOnDisk struct {
	path string
	root string
	size int64
	mod  time.Time
}

func (e *Engine) reconcileFiles(ctx context.Context, c *arr.Client, rep *Report, trace *llmobs.Trace) error {
	tracked, err := c.TrackedFiles(ctx)
	if err != nil {
		return err
	}
	trackedSet := make(map[string]int64, len(tracked))
	for _, f := range tracked {
		trackedSet[filepath.Clean(f.Path)] = f.Size
	}

	roots, err := c.RootFolders(ctx)
	if err != nil {
		return err
	}

	// MISSING: DB references a file that is not on disk -> trigger a rescan so
	// the arr instance corrects its own state (and re-searches if monitored).
	missing := 0
	for _, f := range tracked {
		if _, err := os.Stat(f.Path); err != nil {
			if os.IsNotExist(err) {
				missing++
			}
		}
	}
	if missing > 0 {
		rec := ActionRecord{
			Instance: c.Name(), Item: fmt.Sprintf("%d missing file(s)", missing),
			Action: "rescan", Reason: "DB references files absent on disk; triggering rescan",
		}
		if e.cfg.DryRun {
			rec.Reason = "[dry-run] " + rec.Reason
		} else if err := c.Rescan(ctx); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("rescan %s: %v", c.Name(), err))
		}
		rep.MissingFiles = append(rep.MissingFiles, rec)
	}

	// ORPHANS: files on disk under a root that the DB does not reference.
	if !e.cfg.Safety.DeleteEnabled {
		return nil // orphan handling disabled; missing-file rescan already done
	}

	var orphans []fileOnDisk
	for _, rf := range roots {
		// Only walk roots that are within the configured allowlist.
		if !e.cfg.Safety.WithinAllowedRoot(rf.Path) {
			e.log.Warn("skipping root outside allowlist", "instance", c.Name(), "root", rf.Path)
			continue
		}
		found, walkErr := walkOrphans(rf.Path, trackedSet)
		if walkErr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("walk %s: %v", rf.Path, walkErr))
		}
		orphans = append(orphans, found...)
	}
	if len(orphans) == 0 {
		return nil
	}

	// Pre-filter by hard safety rails BEFORE the LLM ever sees them, and again
	// after. The LLM cannot widen what these rails permit.
	now := time.Now()
	minAge := e.cfg.Safety.MinOrphanAge.Std()
	refIndex := make(map[string]fileOnDisk)
	var candidates []orphanCandidate
	for i, o := range orphans {
		if !e.passesRails(o, now, minAge) {
			continue
		}
		ref := fmt.Sprintf("o-%d", i)
		refIndex[ref] = o
		candidates = append(candidates, orphanCandidate{
			Ref:      ref,
			Path:     o.path,
			Ext:      strings.ToLower(filepath.Ext(o.path)),
			SizeMB:   o.size / (1 << 20),
			AgeHours: ageBucket(now.Sub(o.mod)),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	// Per-run caps shared across all batches.
	var count int
	var bytesMoved int64
	apply := func(d claude.Decision, dspan, parent *llmobs.Span) {
		o, ok := refIndex[d.Ref]
		if !ok {
			return
		}
		if d.Action != "trash" {
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: o.path, Action: "orphan-keep", Reason: d.Reason, Bytes: o.size,
			})
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "kept")
			return
		}
		// Re-check rails post-decision; the LLM cannot override them.
		if !e.passesRails(o, time.Now(), minAge) {
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: o.path, Action: "orphan-blocked",
				Reason: "failed safety rail recheck", Bytes: o.size,
			})
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "blocked_recheck")
			return
		}
		if count >= e.cfg.Safety.MaxDeletesPerRun {
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: o.path, Action: "orphan-blocked",
				Reason: "per-run delete cap reached", Bytes: o.size,
			})
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "capped_deletes")
			return
		}
		if bytesMoved+o.size > e.cfg.Safety.MaxBytesPerRun {
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Instance: c.Name(), Item: o.path, Action: "orphan-blocked",
				Reason: "per-run byte cap reached", Bytes: o.size,
			})
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "capped_bytes")
			return
		}

		rec := ActionRecord{Instance: c.Name(), Item: o.path, Action: "orphan-trash", Reason: d.Reason, Bytes: o.size}
		if e.cfg.DryRun {
			rec.Reason = "[dry-run] " + rec.Reason
			rep.OrphansTrashed = append(rep.OrphansTrashed, rec)
			count++
			bytesMoved += o.size
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "dry_run")
			return
		}
		dst, err := e.bin.Move(o.path, o.root)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("trash %s: %v", o.path, err))
			e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "error")
			return
		}
		rec.Reason = fmt.Sprintf("%s -> %s", d.Reason, dst)
		rep.OrphansTrashed = append(rep.OrphansTrashed, rec)
		count++
		bytesMoved += o.size
		e.log.Info("trashed orphan", "instance", c.Name(), "path", o.path, "dst", dst, "reason", d.Reason)
		e.recordDecision(trace, dspan, parent, "decision:"+d.Ref, o.path, d, "executed")
	}

	// ---- Funnel: tiers 0 and 1 thin the set before the frontier model -------
	// Tier 0 (rules) auto-trash dead-obvious junk via the same destructive path
	// (apply with a synthesized decision) — still rail-gated. Tier 1 (local
	// model) drops only confident "keep"; junk + uncertain + anything unparseable
	// escalate. The frontier model (tier 2) sees only what survives.
	rep.Funnel.Candidates += len(candidates)
	escalated := e.funnelOrphans(ctx, c, rep, trace, candidates, refIndex, apply)

	runBatched(e, ctx, c, rep, trace, "orphan_triage", e.cfg.Claude.OrphanModel, orphanSystemPrompt,
		e.cfg.Safety.MaxCandidatesPerRun, e.cfg.Safety.BatchSize, escalated, apply)
	return nil
}

// funnelQueue applies the tier-1 local prefilter to stuck queue candidates,
// returning only those that must escalate to the frontier model. A local "keep"
// means the item looks like it is still progressing or in a transient state —
// the safe, non-destructive direction — so it is dropped. Junk + uncertain +
// anything unparseable escalate. With the prefilter disabled, all candidates
// escalate unchanged.
func (e *Engine) funnelQueue(
	ctx context.Context, rep *Report, candidates []queueCandidate,
) []queueCandidate {
	if !e.prefilter.Enabled() || len(candidates) == 0 {
		rep.Funnel.Escalated += len(candidates)
		return candidates
	}
	items := make([]local.Item, len(candidates))
	for i, cand := range candidates {
		status := cand.Status
		if cand.ErrorMessage != "" {
			status += "; err=" + cand.ErrorMessage
		}
		items[i] = local.Item{
			Ref:  cand.Ref,
			Text: fmt.Sprintf("%q status=%s tracked=%s age~%dh remaining=%d%%", cand.Title, status, cand.TrackedDownloadStatus, cand.AgeHours, cand.PercentRemaining),
		}
	}
	verdicts := e.prefilter.Classify(ctx, items)

	var escalated []queueCandidate
	for _, cand := range candidates {
		if verdicts[cand.Ref] == local.Keep {
			rep.Funnel.LocalKeep++
			rep.Skipped = append(rep.Skipped, ActionRecord{
				Item: cand.Title, Action: "queue-keep",
				Reason: "tier-1 local prefilter: still progressing / transient",
			})
			continue
		}
		escalated = append(escalated, cand)
	}
	rep.Funnel.Escalated += len(escalated)
	return escalated
}

// funnelOrphans applies tier-0 rules and the tier-1 local prefilter to the
// orphan candidates and returns only those that must escalate to the frontier
// model. Rule-junk is acted on immediately through apply (a synthesized "trash"
// decision, so it still passes every hard rail and is recorded/capped like any
// other). Local "keep" is dropped (the safe direction — nothing destroyed).
// Everything else escalates. With the prefilter disabled, only tier 0 runs and
// the rest escalate unchanged.
func (e *Engine) funnelOrphans(
	ctx context.Context, c *arr.Client, rep *Report, trace *llmobs.Trace,
	candidates []orphanCandidate, refIndex map[string]fileOnDisk,
	apply func(d claude.Decision, dspan, parent *llmobs.Span),
) []orphanCandidate {

	// Tier 0: deterministic rules.
	var afterRules []orphanCandidate
	for _, cand := range candidates {
		o := refIndex[cand.Ref]
		if !e.cfg.Safety.RulesDisabled && classifyOrphanRule(o.path, o.size) == ruleJunk {
			rep.Funnel.RuleJunk++
			apply(claude.Decision{
				Ref:        cand.Ref,
				Action:     "trash",
				Reason:     "tier-0 rule: obvious orphan junk (" + cand.Ext + ")",
				Confidence: 1,
			}, trace.Start(), trace.Root())
			continue
		}
		afterRules = append(afterRules, cand)
	}

	// Bound the set to this run's cap; the frontier model would only act on this
	// many anyway and overflow defers to the next run.
	if max := e.cfg.Safety.MaxCandidatesPerRun; max > 0 && len(afterRules) > max {
		afterRules = afterRules[:max]
	}

	// No tier-1 local prefilter on the orphan path. Orphans are leftover files,
	// so after tier-0 rules skim the obvious junk the residue is genuinely
	// ambiguous — the local model classifies almost none of it as a confident
	// "keep" (it correctly escalates), while the per-batch latency on a shared
	// GPU is real. The rules tier is the orphan-path win; the frontier model
	// adjudicates the rest. (The local tier is used on the queue path instead,
	// where it can spot transient-recover items.)
	rep.Funnel.Escalated += len(afterRules)
	return afterRules
}

// passesRails enforces the hard safety guardrails that no LLM decision can
// override: path allowlist, protected extensions, minimum age, regular file.
func (e *Engine) passesRails(o fileOnDisk, now time.Time, minAge time.Duration) bool {
	if !e.cfg.Safety.WithinAllowedRoot(o.path) {
		return false
	}
	if e.cfg.Safety.IsProtectedExt(o.path) {
		return false
	}
	if now.Sub(o.mod) < minAge {
		return false
	}
	if o.size <= 0 {
		return false
	}
	return true
}

// walkOrphans walks root and returns regular files not present in trackedSet.
func walkOrphans(root string, trackedSet map[string]int64) ([]fileOnDisk, error) {
	var out []fileOnDisk
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries but keep walking.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		clean := filepath.Clean(path)
		if _, tracked := trackedSet[clean]; tracked {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out = append(out, fileOnDisk{
			path: clean,
			root: filepath.Clean(root),
			size: info.Size(),
			mod:  info.ModTime(),
		})
		return nil
	})
	return out, err
}

// ---- State + summary ------------------------------------------------------

func (e *Engine) persist(rep *Report) error {
	if e.cfg.StateFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.cfg.StateFile), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	tmp := e.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return os.Rename(tmp, e.cfg.StateFile)
}

// Summary renders a compact human-readable summary for notifications.
func (r *Report) Summary() string {
	var b strings.Builder
	mode := "LIVE"
	if r.DryRun {
		mode = "DRY-RUN"
	}
	fmt.Fprintf(&b, "arr-reconciler [%s] %s\n", mode, r.Finished.Format(time.RFC3339))
	fmt.Fprintf(&b, "queue removed: %d | orphans trashed: %d | missing handled: %d | skipped: %d | errors: %d | trash purged: %d\n",
		len(r.QueueRemoved), len(r.OrphansTrashed), len(r.MissingFiles), len(r.Skipped), len(r.Errors), r.TrashPurged)
	if f := r.Funnel; f.Candidates > 0 {
		fmt.Fprintf(&b, "funnel: %d candidates -> rule-junk %d, local-keep %d, escalated to model %d\n",
			f.Candidates, f.RuleJunk, f.LocalKeep, f.Escalated)
	}

	var trashedBytes int64
	for _, a := range r.OrphansTrashed {
		trashedBytes += a.Bytes
	}
	if trashedBytes > 0 {
		fmt.Fprintf(&b, "reclaimed (relocated): %.2f GiB\n", float64(trashedBytes)/(1<<30))
	}
	appendActions(&b, "Queue removed", r.QueueRemoved, 10)
	appendActions(&b, "Orphans trashed", r.OrphansTrashed, 10)
	if len(r.Errors) > 0 {
		b.WriteString("Errors:\n")
		sort.Strings(r.Errors)
		for i, e := range r.Errors {
			if i >= 10 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(r.Errors)-10)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	return b.String()
}

func appendActions(b *strings.Builder, header string, recs []ActionRecord, limit int) {
	if len(recs) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", header)
	for i, a := range recs {
		if i >= limit {
			fmt.Fprintf(b, "  ... and %d more\n", len(recs)-limit)
			break
		}
		fmt.Fprintf(b, "  - [%s] %s — %s\n", a.Instance, a.Item, a.Reason)
	}
}
