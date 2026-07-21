package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	userAgent     = "opencode/latest/1.17.15/cli"
	clientHeader  = "cli"
	sessionHexLen = 26
	serverTimeout = 5 * time.Minute
	idleTimeout   = 2 * time.Minute
	cacheTTL      = 5 * time.Minute
)

// fallbackFreeModels is used when models.dev is unreachable and no upstream
// models match the -free suffix heuristic. These IDs are verified free models.
var fallbackFreeModels = []string{
	"big-pickle",
	"deepseek-v4-flash-free",
	"hy3-free",
	"mimo-v2.5-free",
	"nemotron-3-ultra-free",
	"north-mini-code-free",
}

type config struct {
	listen         string
	upstream       string
	modelsDev      string
	quiet          bool
	catalogPath    string // if set, auto-generate Codex model catalog on startup
}

// costCache holds models.dev cost info with a short TTL so we do not refetch the
// (large) catalog on every request, and so an outage degrades gracefully.
type costCache struct {
	mu      sync.Mutex
	entries map[string]costEntry
	expires time.Time
	ok      bool
}

func (c *costCache) get() (map[string]costEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Now().Before(c.expires) {
		return c.entries, true
	}
	return nil, false
}

func (c *costCache) set(entries map[string]costEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = entries
	c.expires = time.Now().Add(cacheTTL)
	c.ok = true
}

// upstreamModelList is the upstream response shape for ID extraction.
type upstreamModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type modelsDevResponse struct {
	OpenCode struct {
		Models map[string]struct {
			Status string `json:"status"`
			Cost   struct {
				Input float64 `json:"input"`
			} `json:"cost"`
		} `json:"models"`
	} `json:"opencode"`
}

type proxy struct {
	config config
	client *http.Client
	logger *log.Logger
	cost   costCache
}

func main() {
	var cfg config
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8787", "bind address")
	flag.StringVar(&cfg.upstream, "upstream", "https://opencode.ai/zen/v1", "upstream gateway base (includes /v1)")
	flag.StringVar(&cfg.modelsDev, "modelsdev", "https://models.dev/api.json", "models.dev api url for cost info")
	flag.BoolVar(&cfg.quiet, "quiet", false, "reduce logging")

	// Auto-setup flags (single binary approach, no external scripts needed)
	setupMode := flag.Bool("setup", false, "run auto-setup (generate catalog, update config) and exit")
	configure := flag.Bool("configure", false, "auto-add model_catalog_json to Codex config.toml and exit")
	verifyMode := flag.Bool("verify", false, "verify the setup (check catalog, codex model list) and exit")
	withBundled := flag.Bool("with-bundled", false, "include bundled GPT models in catalog (default: zen-only, no GPT models)")
	// --zen-only is kept for backward compatibility but is now the default (no-op)
	zenOnly := flag.Bool("zen-only", false, "deprecated: now the default behavior (zen-only catalog, no GPT models)")
	defaultCatalog := os.ExpandEnv("${HOME}/.openinterpreter/zen_model_catalog.json")
	flag.StringVar(&cfg.catalogPath, "catalog-path", defaultCatalog, "auto-generate Codex model catalog at this path on startup")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	if cfg.quiet {
		logger.SetOutput(io.Discard)
	}

	p := &proxy{
		config: cfg,
		client: &http.Client{Timeout: serverTimeout},
		logger: logger,
	}

	// Handle --verify: check the setup
	if *verifyMode {
		p.verifySetup()
		return
	}

	// Handle --configure: add model_catalog_json to Codex config
	if *configure {
		if err := p.configureCodex(); err != nil {
			logger.Printf("configure error: %v", err)
			os.Exit(1)
		}
		logger.Printf("configure complete: model_catalog_json added to Codex config")
		return
	}

	// Handle --setup: generate catalog + configure + exit
	if *setupMode {
		// --zen-only is now the default; warn if still used
		if *zenOnly {
			logger.Printf("note: --zen-only is now the default behavior (no --with-bundled needed)")
		}
		// Default is zen-only (no bundled GPT models). --with-bundled includes them.
		p.generateCatalogWithOpts(*withBundled)
		if err := p.configureCodex(); err != nil {
			logger.Printf("configure error: %v", err)
		}
		// Print detailed summary
		p.printSetupSummary(*withBundled)
		return
	}

	// Default mode: auto-generate model catalog on startup
	p.generateCatalog()
	// Periodically refresh the catalog to stay in sync with upstream changes.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			p.generateCatalog()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)

	server := &http.Server{
		Addr:         cfg.listen,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  serverTimeout,
		WriteTimeout: serverTimeout,
		IdleTimeout:  idleTimeout,
	}

	log.New(os.Stderr, "", log.LstdFlags).Printf("zen listening on %s -> %s", cfg.listen, cfg.upstream)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func normalizePath(path string) string {
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	normalized := normalizePath(r.URL.Path)

	switch {
	case r.Method == http.MethodGet && normalized == "/models":
		p.handleModels(w, r, normalized)
	case r.Method == http.MethodPost:
		p.handlePost(w, r, normalized)
	default:
		p.forward(w, r, normalized, nil, "")
	}
}

// handleModels fetches the upstream model list once, simultaneously extracting
// IDs for free-model detection and raw JSON entries for passthrough filtering.
// This preserves all upstream fields (created, owned_by, etc.) while avoiding
// redundant upstream calls that could fail independently.
func (p *proxy) handleModels(w http.ResponseWriter, r *http.Request, normalized string) {
	// Single upstream call: fetch raw JSON and extract IDs simultaneously.
	req, err := http.NewRequest(http.MethodGet, p.config.upstream+"/models", nil)
	if err != nil {
		p.logger.Printf("path=%s error building request: %v", r.URL.Path, err)
		writeError(w, http.StatusBadGateway, "failed to build upstream request", "upstream_error", "")
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("x-opencode-client", clientHeader)
	req.Header.Set("x-opencode-session", randomHex(sessionHexLen))
	req.Header.Set("x-opencode-project", randomHex(sessionHexLen))
	req.Header.Set("x-opencode-request", randomHex(sessionHexLen))
	resp, err := p.client.Do(req)
	if err != nil {
		// Upstream unreachable: fall back to known free model list.
		p.logger.Printf("path=%s upstream=unreachable (%v); using fallback", r.URL.Path, err)
		p.writeFallbackModels(w, r)
		return
	}
	defer resp.Body.Close()

	var rawResp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
		p.logger.Printf("path=%s upstream=decode error (%v); using fallback", r.URL.Path, err)
		p.writeFallbackModels(w, r)
		return
	}

	// Extract IDs from raw entries and determine which are free.
	allIDs := make([]string, 0, len(rawResp.Data))
	idFromRaw := make(map[string]json.RawMessage, len(rawResp.Data))
	for _, raw := range rawResp.Data {
		var partial struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &partial); err != nil || partial.ID == "" {
			continue
		}
		allIDs = append(allIDs, partial.ID)
		idFromRaw[partial.ID] = raw
	}

	costInfo := p.costInfo()
	freeSet := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		if isFree(id, costInfo) {
			freeSet[id] = true
		}
	}

	// Filter raw entries to only free models.
	filtered := make([]json.RawMessage, 0, len(freeSet))
	for id := range freeSet {
		if raw, ok := idFromRaw[id]; ok {
			filtered = append(filtered, raw)
		}
	}

	// If no free models detected, fall back to the hardcoded fallback list.
	// This matches the fallback behavior in freeModels() so that GET /models
	// and POST validation are consistent.
	if len(filtered) == 0 {
		p.logger.Printf("path=%s no free models detected from upstream; using fallback list", r.URL.Path)
		p.writeFallbackModels(w, r)
		return
	}

	respBytes, err := json.Marshal(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{
		Object: "list",
		Data:   filtered,
	})
	if err != nil {
		p.logger.Printf("path=%s error marshaling response: %v", r.URL.Path, err)
		writeError(w, http.StatusInternalServerError, "failed to build response", "internal_error", "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
	p.logger.Printf("path=%s model=- upstream=intercepted free_count=%d", r.URL.Path, len(filtered))
}

// writeFallbackModels constructs a model list from the verified fallback list
// when the upstream is unreachable or returns unparseable data.
func (p *proxy) writeFallbackModels(w http.ResponseWriter, r *http.Request) {
	freeIDs := p.freeModelsFromFallback()
	data := make([]json.RawMessage, 0, len(freeIDs))
	for _, id := range freeIDs {
		entry, _ := json.Marshal(struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}{
			ID:      id,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "opencode",
		})
		data = append(data, entry)
	}
	respBytes, _ := json.Marshal(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{
		Object: "list",
		Data:   data,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
	p.logger.Printf("path=%s model=- upstream=fallback free_count=%d", r.URL.Path, len(freeIDs))
}

// freeModelsFromFallback returns the verified fallback free model list.
func (p *proxy) freeModelsFromFallback() []string {
	return append([]string(nil), fallbackFreeModels...)
}

// handlePost enforces the free-only rule on any POST that carries a model field
// (chat/completions, responses, embeddings, …), rewrites the model id to its bare
// form, then forwards the request upstream. Requests without a model field are
// forwarded untouched.
func (p *proxy) handlePost(w http.ResponseWriter, r *http.Request, normalized string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request", "")
		return
	}

	var reqBody map[string]interface{}
	_ = json.Unmarshal(body, &reqBody)

	rawModel, _ := reqBody["model"].(string)
	model := stripProviderPrefix(rawModel)
	if model != "" {
		freeIDs, err := p.freeModels()
		if err != nil {
			p.logger.Printf("path=%s model=%s error computing free models: %v", r.URL.Path, model, err)
			writeError(w, http.StatusBadGateway, "failed to compute free models: "+err.Error(), "upstream_error", "")
			return
		}
		if !contains(freeIDs, model) {
			msg := "model " + rawModel + " is not a free model; this proxy only serves free OpenCode Zen models"
			p.logger.Printf("path=%s model=%s upstream=- rejected=free-only", r.URL.Path, model)
			writeError(w, http.StatusBadRequest, msg, "model_not_available", "model")
			return
		}
		reqBody["model"] = model
		if rewritten, err := json.Marshal(reqBody); err == nil {
			body = rewritten
		}
	}

	p.forward(w, r, normalized, body, model)
}

// stripProviderPrefix removes a leading "provider/" segment some clients send
// (e.g. "zenfree/hy3-free"); OpenCode Zen expects the bare model id ("hy3-free").
func stripProviderPrefix(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// hopHeaders are not forwarded to the upstream; they are connection-specific or
// would conflict with the rebuilt request. Keys are lower-cased: lookups must use
// strings.ToLower(k).
var hopHeaders = map[string]bool{
	"authorization":     true,
	"content-length":    true,
	"connection":        true,
	"transfer-encoding": true,
	"trailer":           true,
	"host":              true,
	"accept-encoding":   true,
}

// forward copies the incoming request (minus credential and hop-by-hop headers) to
// the upstream OpenCode Zen gateway, overlaying the OpenCode client identity so the
// request mirrors one sent by the official `opencode` CLI. Any header the client
// sends (including protocol-version markers) is preserved, so the proxy stays
// compatible with the current Zen protocol; only the key is stripped and the
// opencode identity is asserted.
func (p *proxy) forward(w http.ResponseWriter, r *http.Request, normalized string, body []byte, model string) {
	target := p.config.upstream + normalized

	var bodyReader io.Reader
	switch {
	case body != nil:
		bodyReader = bytes.NewReader(body)
	case r.Body != nil:
		bodyReader = r.Body
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to build upstream request: "+err.Error(), "upstream_error", "")
		return
	}

	for k, vals := range r.Header {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			upstreamReq.Header.Add(k, v)
		}
	}

	upstreamReq.Header.Set("User-Agent", userAgent)
	upstreamReq.Header.Set("x-opencode-client", clientHeader)
	upstreamReq.Header.Set("x-opencode-session", randomHex(sessionHexLen))
	upstreamReq.Header.Set("x-opencode-project", randomHex(sessionHexLen))
	upstreamReq.Header.Set("x-opencode-request", randomHex(sessionHexLen))

	resp, err := p.client.Do(upstreamReq)
	if err != nil {
		p.logger.Printf("path=%s model=%s upstream=error: %v", r.URL.Path, model, err)
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "upstream_error", "")
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	_, _ = io.Copy(flushWriter{w}, resp.Body)

	p.logger.Printf("path=%s model=%s upstream=%d", r.URL.Path, model, resp.StatusCode)
}

type flushWriter struct {
	w http.ResponseWriter
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if flusher, ok := fw.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (p *proxy) freeModels() ([]string, error) {
	liveIDs, err := p.fetchLiveModels()
	if err != nil {
		// Upstream unreachable: use verified fallback list.
		p.logger.Printf("path=- upstream=unreachable (%v); using fallback model list", err)
		return append([]string(nil), fallbackFreeModels...), nil
	}
	costInfo := p.costInfo()

	free := make([]string, 0, len(liveIDs))
	for _, id := range liveIDs {
		if isFree(id, costInfo) {
			free = append(free, id)
		}
	}

	// If cost info is empty and no models match -free suffix, fall back.
	if len(free) == 0 {
		p.logger.Printf("path=- no free models detected from upstream; using fallback list")
		return append([]string(nil), fallbackFreeModels...), nil
	}
	return free, nil
}

// costInfo returns models.dev cost info, using a short-lived cache. If models.dev
// is unreachable, it returns an empty map (non-fatal) so free detection falls back
// to the "-free" suffix rule, and the live model list still gates availability.
func (p *proxy) costInfo() map[string]costEntry {
	if info, ok := p.cost.get(); ok {
		return info
	}
	info, err := p.fetchCostInfo()
	if err != nil {
		p.logger.Printf("models.dev unavailable (%v); falling back to -free suffix rule", err)
		return map[string]costEntry{}
	}
	p.cost.set(info)
	return info
}

type costEntry struct {
	inputCost float64
	status    string
}

func (p *proxy) fetchLiveModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, p.config.upstream+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("x-opencode-client", clientHeader)
	req.Header.Set("x-opencode-session", randomHex(sessionHexLen))
	req.Header.Set("x-opencode-project", randomHex(sessionHexLen))
	req.Header.Set("x-opencode-request", randomHex(sessionHexLen))
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list upstreamModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (p *proxy) fetchCostInfo() (map[string]costEntry, error) {
	req, err := http.NewRequest(http.MethodGet, p.config.modelsDev, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed modelsDevResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	info := make(map[string]costEntry, len(parsed.OpenCode.Models))
	for id, entry := range parsed.OpenCode.Models {
		info[id] = costEntry{
			inputCost: entry.Cost.Input,
			status:    entry.Status,
		}
	}
	return info, nil
}

func isFree(id string, info map[string]costEntry) bool {
	if entry, ok := info[id]; ok {
		// Cost info exists: trust it. Models with inputCost > 0 or status "deprecated" are not free.
		return entry.inputCost == 0 && entry.status != "deprecated"
	}
	// No cost info for this model (models.dev may lack it): fall back to -free suffix heuristic.
	return strings.HasSuffix(id, "-free")
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func randomHex(length int) string {
	buf := make([]byte, (length+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", length)
	}
	return hex.EncodeToString(buf)[:length]
}

// modelEntryTemplate is the base template for free model entries in the catalog.
// These fields match the Codex model catalog schema so that custom models appear
// correctly in the Codex UI model picker.
var modelEntryTemplate = map[string]interface{}{
	"supported_reasoning_levels": []map[string]interface{}{
		{"effort": "low", "description": "Fast responses with lighter reasoning"},
		{"effort": "medium", "description": "Balances speed and reasoning depth"},
		{"effort": "high", "description": "Maximum reasoning depth for complex problems"},
	},
	"base_instructions":           "You are a helpful assistant powered by a free OpenCode Zen model via the local proxy.",
	"context_window":              128000,
	"max_context_window":          128000,
	"default_reasoning_level":     "low",
	"reasoning_control":           "none",
	"shell_type":                  "shell_command",
	"visibility":                  "list",
	"supported_in_api":            true,
	"priority":                    10,
	"additional_speed_tiers":      []interface{}{},
	"service_tiers":               []map[string]interface{}{{"id": "default", "name": "Standard", "description": "Standard speed"}},
	"upgrade":                     nil,
	"include_skills_usage_instructions": false,
	"supports_reasoning_summaries":      true,
	"default_reasoning_summary":         "none",
	"support_verbosity":                 true,
	"default_verbosity":                 "low",
	"apply_patch_tool_type":             "freeform",
	"web_search_tool_type":              "text_and_image",
	"truncation_policy":                 map[string]interface{}{"mode": "tokens", "limit": 10000},
	"supports_parallel_tool_calls":      true,
	"supports_image_detail_original":    true,
	"effective_context_window_percent":  95,
	"experimental_supported_tools":      []interface{}{},
	"input_modalities":                  []interface{}{"text", "image"},
	"supports_search_tool":              true,
	"use_responses_lite":                true,
}

// generateCatalog generates the Codex model catalog file by merging the bundled
// catalog (from `codex debug models --bundled`) with the proxy's free model list.
// This allows Codex to display the free models in its UI picker without needing
// dynamic remote model discovery (which is removed in Codex v0.0.34).
//
// The catalog is written to p.config.catalogPath. If codex is unavailable, or if
// the upstream free model list cannot be fetched, the function logs a warning and
// returns without writing (falling back to any existing catalog).
func (p *proxy) generateCatalogWithOpts(withBundled bool) {
	p.logger.Printf("catalog-path=%s generating model catalog...", p.config.catalogPath)

	// Ensure catalogPath is set
	if p.config.catalogPath == "" {
		p.logger.Printf("catalog-path unset: no catalog path, skipping")
		return
	}

	// Step 1: Optionally get bundled catalog from Codex.
	// By default, only free zen models are included (no bundled GPT models).
	// Use --with-bundled to include Codex's bundled GPT models in the catalog.
	var bundledModels []map[string]interface{}
	if !withBundled {
		p.logger.Printf("catalog-path=%s zen-only mode: skipping bundled GPT models", p.config.catalogPath)
		bundledModels = []map[string]interface{}{}
	} else {
		// Use --bundled to bypass any custom model_catalog_json (avoid circular dependency).
		cmd := exec.Command("codex", "debug", "models", "--bundled")
		output, err := cmd.Output()
		if err != nil {
			p.logger.Printf("catalog-path=%s warning: cannot get bundled catalog (codex not installed?): %v", p.config.catalogPath, err)
			bundledModels = []map[string]interface{}{}
		} else {
			var bundled struct {
				Models []map[string]interface{} `json:"models"`
			}
			if err := json.Unmarshal(output, &bundled); err != nil {
				p.logger.Printf("catalog-path=%s warning: cannot parse bundled catalog: %v", p.config.catalogPath, err)
				bundledModels = []map[string]interface{}{}
			} else {
				bundledModels = bundled.Models
			}
		}
	}

	// Collect existing slugs to avoid duplicates.
	existingSlugs := make(map[string]bool, len(bundledModels))
	for _, m := range bundledModels {
		if slug, ok := m["slug"].(string); ok {
			existingSlugs[slug] = true
		}
	}

	// Step 2: Get free model list from upstream.
	freeIDs, err := p.freeModels()
	if err != nil {
		p.logger.Printf("catalog-path=%s warning: could not determine free models: %v; using fallback list", p.config.catalogPath, err)
		freeIDs = p.freeModelsFromFallback()
	}

	// Step 3: Create model entries for each free model.
	var newEntries []map[string]interface{}
	for _, id := range freeIDs {
		if existingSlugs[id] {
			p.logger.Printf("catalog-path=%s skipping (already in bundled): %s", p.config.catalogPath, id)
			continue
		}
		entry := make(map[string]interface{})
		for k, v := range modelEntryTemplate {
			entry[k] = v
		}
		entry["slug"] = id
		// Generate display name: replace hyphens with spaces, capitalize words
		words := strings.Fields(strings.ReplaceAll(id, "-", " "))
		for i, w := range words {
			if len(w) > 0 {
				words[i] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		entry["display_name"] = strings.Join(words, " ")
		entry["description"] = "Free OpenCode Zen model: " + id
		entry["availability_nux"] = map[string]interface{}{
			"message": "Free OpenCode Zen model. No API key required.",
		}
		newEntries = append(newEntries, entry)
		existingSlugs[id] = true
		p.logger.Printf("catalog-path=%s adding free model: %s", p.config.catalogPath, id)
	}

	// Step 4: Merge and write.
	combined := map[string]interface{}{
		"models": append(bundledModels, newEntries...),
	}

	// Ensure directory exists. Handle edge case where path has no directory separator.
	dir := p.config.catalogPath
	if lastSlash := strings.LastIndex(p.config.catalogPath, "/"); lastSlash >= 0 {
		dir = p.config.catalogPath[:lastSlash]
	} else {
		dir = "." // fallback to current directory
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		p.logger.Printf("catalog-path=%s error creating directory: %v", p.config.catalogPath, err)
		return
	}

	data, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		p.logger.Printf("catalog-path=%s error marshaling catalog: %v", p.config.catalogPath, err)
		return
	}

	if err := os.WriteFile(p.config.catalogPath, data, 0644); err != nil {
		p.logger.Printf("catalog-path=%s error writing catalog: %v", p.config.catalogPath, err)
		return
	}

	p.logger.Printf("catalog-path=%s generated: %d bundled + %d free = %d models",
		p.config.catalogPath, len(bundledModels), len(newEntries), len(combined["models"].([]map[string]interface{})))
}

// generateCatalog wraps generateCatalogWithOpts with bundled models excluded (default zen-only).
func (p *proxy) generateCatalog() {
	p.generateCatalogWithOpts(false)
}

// verifySetup checks the current setup state: catalog file exists, codex can parse it,
// and reports the model list. Exits with code 1 if anything is wrong.
func (p *proxy) verifySetup() {
	home := os.Getenv("HOME")
	if home == "" {
		p.logger.Printf("verify: HOME not set")
		os.Exit(1)
	}
	p.openinterpreterDir()

	catalogPath := p.config.catalogPath
	if catalogPath == "" {
		catalogPath = home + "/.openinterpreter/zen_model_catalog.json"
	}
	configPath := home + "/.openinterpreter/config.toml"

	p.logger.Printf("=== Zen Setup Verification ===")
	p.logger.Printf("")

	// Check catalog file
	if _, err := os.Stat(catalogPath); err == nil {
		data, err := os.ReadFile(catalogPath)
		if err == nil {
			var parsed struct {
				Models []map[string]interface{} `json:"models"`
			}
			if err := json.Unmarshal(data, &parsed); err != nil {
				p.logger.Printf("Catalog:   EXIST but INVALID JSON (%v)", err)
			} else {
				freeCount := 0
				for _, m := range parsed.Models {
					if slug, ok := m["slug"].(string); ok {
						if strings.Contains(slug, "-free") || slug == "big-pickle" {
							freeCount++
						}
					}
				}
				p.logger.Printf("Catalog:   %s", catalogPath)
				p.logger.Printf("  Exists:  YES (%d models, %d free)", len(parsed.Models), freeCount)
			}
		} else {
			p.logger.Printf("Catalog:   %s (UNREADABLE: %v)", catalogPath, err)
		}
	} else {
		p.logger.Printf("Catalog:   %s (NOT FOUND)", catalogPath)
	}

	// Check config file
	if _, err := os.Stat(configPath); err == nil {
		data, _ := os.ReadFile(configPath)
		hasCatalogSetting := strings.Contains(string(data), "model_catalog_json")
		currentProvider := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model_provider = ") {
				currentProvider = strings.Trim(strings.TrimPrefix(line, "model_provider = "), "\"")
			}
		}
		p.logger.Printf("Config:    %s", configPath)
		p.logger.Printf("  Provider: %s", currentProvider)
		p.logger.Printf("  Catalog:  %s", map[bool]string{true: "configured", false: "NOT configured"}[hasCatalogSetting])
	} else {
		p.logger.Printf("Config:    %s (NOT FOUND)", configPath)
	}

	// Check codex model list
	cmd := exec.Command("codex", "debug", "models")
	output, err := cmd.Output()
	if err != nil {
		p.logger.Printf("Codex:     CLI not found or error: %v", err)
	} else {
		var parsed struct {
			Models []map[string]interface{} `json:"models"`
		}
		if err := json.Unmarshal(output, &parsed); err != nil {
			p.logger.Printf("Codex:     model list unparseable: %v", err)
		} else {
			freeCount := 0
			p.logger.Printf("Codex:     model list (%d models):", len(parsed.Models))
			for _, m := range parsed.Models {
				slug, _ := m["slug"].(string)
				isFree := strings.Contains(slug, "-free") || slug == "big-pickle"
				marker := ""
				if isFree {
					marker = " (free)"
					freeCount++
				}
				p.logger.Printf("  - %s%s", slug, marker)
			}
			p.logger.Printf("")
			p.logger.Printf("Free models visible: %d/%d", freeCount, len(parsed.Models))
		}
	}

	// Check proxy
	resp, err := http.Get("http://" + p.config.listen + "/v1/models")
	if err != nil {
		p.logger.Printf("Proxy:     http://%s (NOT RESPONDING: %v)", p.config.listen, err)
	} else {
		defer resp.Body.Close()
		p.logger.Printf("Proxy:     http://%s (UP - %s)", p.config.listen, resp.Status)
	}

	p.logger.Printf("")
	p.logger.Printf("=== Note ===")
	p.logger.Printf("Codex v0.0.34 has 'remote_models' feature REMOVED. This means Codex")
	p.logger.Printf("NEVER queries any provider's /v1/models API. The model_catalog_json")
	p.logger.Printf("workaround is the only way to add custom models. All models shown")
	p.logger.Printf("above come from the catalog file, not from the live API.")
	p.logger.Printf("")
	p.logger.Printf("To regenerate: run './zen --setup' to regenerate the catalog.")
}

// printSetupSummary prints a detailed summary after --setup completes.
func (p *proxy) printSetupSummary(withBundled bool) {
	home := os.Getenv("HOME")
	if home == "" {
		return
	}
	catalogPath := p.config.catalogPath
	if catalogPath == "" {
		catalogPath = home + "/.openinterpreter/zen_model_catalog.json"
	}

	p.logger.Printf("=== Zen Setup Complete ===")
	p.logger.Printf("")
	p.logger.Printf("Catalog: %s", catalogPath)

	// Read back the catalog to count
	if data, err := os.ReadFile(catalogPath); err == nil {
		var parsed struct {
			Models []map[string]interface{} `json:"models"`
		}
		if err := json.Unmarshal(data, &parsed); err == nil {
			freeCount := 0
			for _, m := range parsed.Models {
				if slug, ok := m["slug"].(string); ok {
					if strings.Contains(slug, "-free") || slug == "big-pickle" {
						freeCount++
					}
				}
			}
			p.logger.Printf("Models:  %d total (%d free)", len(parsed.Models), freeCount)
			for _, m := range parsed.Models {
				slug, _ := m["slug"].(string)
				isFree := strings.Contains(slug, "-free") || slug == "big-pickle"
				if isFree {
					p.logger.Printf("  + %s (free)", slug)
				}
			}
		}
	}

	p.logger.Printf("")
	if withBundled {
		p.logger.Printf("NOTE: The catalog includes bundled GPT models from Codex.")
		p.logger.Printf("  This is because model_catalog_json REPLACES Codex's built-in")
		p.logger.Printf("  catalog. Bundled models are included so other providers (OpenAI,")
		p.logger.Printf("  Anthropic, etc.) still work. The default is zen-only:")
		p.logger.Printf("    ./zen --setup")
		p.logger.Printf("  (no --with-bundled needed for zen-only catalog).")
	} else {
		p.logger.Printf("Catalog is zen-only (no bundled GPT models). Only free models")
		p.logger.Printf("will appear in Codex's model list.")
		p.logger.Printf("  If you need bundled models for other providers, use:")
		p.logger.Printf("    ./zen --setup --with-bundled")
	}
	p.logger.Printf("")
	p.logger.Printf("To verify:  ./zen --verify")
	p.logger.Printf("To start:   ./zen")
}

// openinterpreterDir ensures ~/.openinterpreter exists.
func (p *proxy) openinterpreterDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	dir := home + "/.openinterpreter"
	os.MkdirAll(dir, 0755)
	return dir
}

// configureCodex ensures the Codex config.toml has the zen provider and model_catalog_json.
// This auto-configures Codex for the zen proxy with no manual editing.
func (p *proxy) configureCodex() error {
	home := os.Getenv("HOME")
	if home == "" {
		return fmt.Errorf("HOME not set")
	}
	p.openinterpreterDir()
	configPath := home + "/.openinterpreter/config.toml"
	catalogPath := p.config.catalogPath
	if catalogPath == "" {
		catalogPath = home + "/.openinterpreter/zen_model_catalog.json"
	}

	// Read existing config (or start fresh)
	data, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("reading config: %w", err)
		}
		// Config doesn't exist yet: start with minimal content
		data = []byte{}
	}

	configStr := string(data)
	needsWrite := false

	// 1. Set model_provider to zen if not already set
	// Check for model_provider anywhere in config (including commented out)
	if !strings.Contains(configStr, "\nmodel_provider = ") && !strings.HasPrefix(configStr, "model_provider = ") {
		prefix := "model_provider = \"zen\"\n"
		// Also set default model, but only if model is not already configured
		if !strings.Contains(configStr, "\nmodel = ") && !strings.HasPrefix(configStr, "model = ") {
			prefix += "model = \"deepseek-v4-flash-free\"\n"
		}
		if idx := strings.Index(configStr, "\n["); idx >= 0 {
			configStr = prefix + configStr[:idx+1] + configStr[idx+1:]
		} else {
			configStr = prefix + configStr
		}
		needsWrite = true
		p.logger.Printf("configure: set model_provider = zen")
	}

	// 2. Ensure [model_providers.zen] section exists
	if !strings.Contains(configStr, "[model_providers.zen]") {
		providerSection := "\n[model_providers.zen]\nname = \"zen\"\nbase_url = \"http://127.0.0.1:8787/v1\"\napi_key = \"sk-zen\"\nwire_api = \"chat\"\nrequest_max_retries = 7\nstream_max_retries = 9\n"
		configStr += providerSection
		needsWrite = true
		p.logger.Printf("configure: added [model_providers.zen] section")
	}

	// 3. Ensure model_catalog_json is set
	if !strings.Contains(configStr, "model_catalog_json") {
		line := fmt.Sprintf("model_catalog_json = %q\n", catalogPath)
		// Insert before first [section] or append at end
		if idx := strings.Index(configStr, "\n["); idx >= 0 {
			configStr = configStr[:idx+1] + line + configStr[idx+1:]
		} else {
			configStr = configStr + "\n" + line
		}
		needsWrite = true
		p.logger.Printf("configure: added model_catalog_json to config")
	} else {
		p.logger.Printf("configure: model_catalog_json already in config")
	}

	if needsWrite {
		if err := os.WriteFile(configPath, []byte(configStr), 0644); err != nil {
			return fmt.Errorf("writing config: %w", err)
		}
		p.logger.Printf("configure: updated %s", configPath)
	} else {
		p.logger.Printf("configure: config already complete, no changes needed")
	}
	return nil
}

// writeError writes an OpenAI-compatible JSON error response.
// This improves standards compliance so clients can parse errors programmatically.
func writeError(w http.ResponseWriter, status int, message, code, param string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	}
	if param != "" {
		resp["error"].(map[string]interface{})["param"] = param
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// corsMiddleware adds CORS headers for browser-based clients.
// This is standards-compliant behavior for OpenAI-compatible endpoints.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
