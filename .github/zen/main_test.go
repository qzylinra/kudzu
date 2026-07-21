package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- normalizePath ----------

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/v1", "/"},
		{"/v1/models", "/models"},
		{"/v1/chat/completions", "/chat/completions"},
		{"/v1/", "/"},
		{"/models", "/models"},
		{"/v1/models/extra", "/models/extra"},
		{"/", "/"},
		{"/other", "/other"},
	}
	for _, tc := range tests {
		got := normalizePath(tc.input)
		if got != tc.expected {
			t.Errorf("normalizePath(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

// ---------- isFree ----------

func TestIsFree(t *testing.T) {
	tests := []struct {
		id       string
		info     map[string]costEntry
		expected bool
	}{
		{"model-a", map[string]costEntry{"model-a": {inputCost: 0, status: "active"}}, true},
		{"model-a", map[string]costEntry{"model-a": {inputCost: 0.5, status: "active"}}, false},
		{"model-a", map[string]costEntry{"model-a": {inputCost: 0, status: "deprecated"}}, false},
		{"model-a", map[string]costEntry{"model-a": {inputCost: 5, status: "deprecated"}}, false},
		{"hy3-free", map[string]costEntry{}, true},
		{"gpt-4", map[string]costEntry{}, false},
		{"deepseek-v4-flash-free", map[string]costEntry{}, true},
		{"north-mini-code-free", map[string]costEntry{}, true},
		{"claude-3", map[string]costEntry{}, false},
		{"", map[string]costEntry{}, false},
		{"-free", map[string]costEntry{}, true},
	}
	for _, tc := range tests {
		got := isFree(tc.id, tc.info)
		if got != tc.expected {
			t.Errorf("isFree(%q, %v) = %v; want %v", tc.id, tc.info, got, tc.expected)
		}
	}
}

// ---------- stripProviderPrefix ----------

func TestStripProviderPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"zenfree/hy3-free", "hy3-free"},
		{"provider/model-name", "model-name"},
		{"hy3-free", "hy3-free"},
		{"", ""},
		{"a/b/c", "b/c"},
	}
	for _, tc := range tests {
		got := stripProviderPrefix(tc.input)
		if got != tc.expected {
			t.Errorf("stripProviderPrefix(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

// ---------- contains ----------

func TestContains(t *testing.T) {
	items := []string{"a", "b", "c"}
	if !contains(items, "a") {
		t.Error("contains should find 'a'")
	}
	if !contains(items, "c") {
		t.Error("contains should find 'c'")
	}
	if contains(items, "d") {
		t.Error("contains should not find 'd'")
	}
	if contains([]string{}, "a") {
		t.Error("contains on empty slice should return false")
	}
}

// ---------- randomHex ----------

func TestRandomHex(t *testing.T) {
	for _, length := range []int{0, 1, 10, 26, 100} {
		result := randomHex(length)
		if len(result) != length {
			t.Errorf("randomHex(%d) returned length %d", length, len(result))
		}
		for _, c := range result {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("randomHex(%d) contains non-hex char %c", length, c)
			}
		}
	}
}

// ---------- freeModelsFromFallback ----------

func TestFreeModelsFromFallback(t *testing.T) {
	p := &proxy{logger: log.New(io.Discard, "", 0)}
	result := p.freeModelsFromFallback()

	if len(result) != len(fallbackFreeModels) {
		t.Errorf("freeModelsFromFallback returned %d items; want %d", len(result), len(fallbackFreeModels))
	}
	for i, id := range fallbackFreeModels {
		if result[i] != id {
			t.Errorf("freeModelsFromFallback[%d] = %q; want %q", i, result[i], id)
		}
	}

	if len(result) > 0 {
		result[0] = "modified"
		if fallbackFreeModels[0] == "modified" {
			t.Error("freeModelsFromFallback should return a copy")
		}
	}
}

// ---------- helpers ----------

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func startTestProxy(t *testing.T, upstreamModels []modelEntry) (*httptest.Server, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			data, _ := json.Marshal(map[string]interface{}{
				"object": "list",
				"data":   upstreamModels,
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"test"}`))
		}
	}))

	p := &proxy{
		config: config{
			upstream: upstream.URL,
			quiet:    true,
		},
		client: &http.Client{Timeout: 5 * time.Second},
		logger: log.New(io.Discard, "", 0),
	}
	p.cost = costCache{}

	proxyServer := httptest.NewServer(http.HandlerFunc(p.handle))
	return proxyServer, upstream
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, resp.ContentLength)
	_, _ = resp.Body.Read(buf)
	return strings.TrimSpace(string(buf))
}

// ---------- handleModels tests ----------

func TestHandleModelsReturnsFreeModels(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model", Created: 1000, OwnedBy: "opencode"},
		{ID: "gpt-4", Object: "model", Created: 1001, OwnedBy: "openai"},
		{ID: "deepseek-v4-flash-free", Object: "model", Created: 1002, OwnedBy: "deepseek"},
		{ID: "claude-opus", Object: "model", Created: 1003, OwnedBy: "anthropic"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.Object != "list" {
		t.Errorf("object = %q; want %q", result.Object, "list")
	}

	gotIDs := make(map[string]bool)
	for _, raw := range result.Data {
		var entry struct{ ID string `json:"id"` }
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("failed to decode entry: %v", err)
		}
		gotIDs[entry.ID] = true
	}

	expectedFree := []string{"hy3-free", "deepseek-v4-flash-free"}
	if len(gotIDs) != len(expectedFree) {
		t.Fatalf("got %d models; want %d models. got: %v", len(gotIDs), len(expectedFree), gotIDs)
	}
	for _, id := range expectedFree {
		if !gotIDs[id] {
			t.Errorf("missing expected free model: %s", id)
		}
	}
	if gotIDs["gpt-4"] {
		t.Error("gpt-4 should NOT be in free model list")
	}
	if gotIDs["claude-opus"] {
		t.Error("claude-opus should NOT be in free model list")
	}
}

func TestHandleModelsFallbackWhenNoFreeModels(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "gpt-4", Object: "model", Created: 1001, OwnedBy: "openai"},
		{ID: "claude-opus", Object: "model", Created: 1003, OwnedBy: "anthropic"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	var ids []string
	for _, raw := range result.Data {
		var entry struct{ ID string `json:"id"` }
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("failed to decode entry: %v", err)
		}
		ids = append(ids, entry.ID)
	}

	if len(ids) != len(fallbackFreeModels) {
		t.Fatalf("got %d models; want %d (fallback list). IDs: %v", len(ids), len(fallbackFreeModels), ids)
	}
	for i, id := range fallbackFreeModels {
		if ids[i] != id {
			t.Errorf("model[%d] = %q; want %q", i, ids[i], id)
		}
	}
}

func TestHandleModelsUpstreamUnreachable(t *testing.T) {
	p := &proxy{
		config: config{
			upstream: "http://127.0.0.1:1",
			quiet:    true,
		},
		client: &http.Client{Timeout: time.Second},
		logger: log.New(io.Discard, "", 0),
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handle))
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	var ids []string
	for _, raw := range result.Data {
		var entry struct{ ID string `json:"id"` }
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("failed to decode entry: %v", err)
		}
		ids = append(ids, entry.ID)
	}

	if len(ids) != len(fallbackFreeModels) {
		t.Fatalf("got %d models; want %d (fallback). IDs: %v", len(ids), len(fallbackFreeModels), ids)
	}
}

func TestHandleModelsUpstreamUnparseable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	defer upstream.Close()

	p := &proxy{
		config: config{
			upstream: upstream.URL,
			quiet:    true,
		},
		client: &http.Client{Timeout: 5 * time.Second},
		logger: log.New(io.Discard, "", 0),
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handle))
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	var ids []string
	for _, raw := range result.Data {
		var entry struct{ ID string `json:"id"` }
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("failed to decode entry: %v", err)
		}
		ids = append(ids, entry.ID)
	}

	if len(ids) != len(fallbackFreeModels) {
		t.Fatalf("got %d models; want %d (fallback). IDs: %v", len(ids), len(fallbackFreeModels), ids)
	}
}

func TestHandleModelsPreservesUpstreamFields(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model", Created: 1234567890, OwnedBy: "test-org"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(result.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.Data))
	}

	var entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(result.Data[0], &entry); err != nil {
		t.Fatalf("failed to decode entry: %v", err)
	}

	if entry.ID != "hy3-free" {
		t.Errorf("id = %q; want %q", entry.ID, "hy3-free")
	}
	if entry.Object != "model" {
		t.Errorf("object = %q; want %q", entry.Object, "model")
	}
	if entry.Created != 1234567890 {
		t.Errorf("created = %d; want %d", entry.Created, 1234567890)
	}
	if entry.OwnedBy != "test-org" {
		t.Errorf("owned_by = %q; want %q", entry.OwnedBy, "test-org")
	}
}

func TestHandleModelsSetsOpenCodeHeaders(t *testing.T) {
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header
		data, _ := json.Marshal(map[string]interface{}{
			"data": []modelEntry{
				{ID: "hy3-free", Object: "model"},
			},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	p := &proxy{
		config: config{
			upstream: upstream.URL,
			quiet:    true,
		},
		client: &http.Client{Timeout: 5 * time.Second},
		logger: log.New(io.Discard, "", 0),
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handle))
	defer proxyServer.Close()

	_, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}

	if capturedHeaders.Get("User-Agent") != userAgent {
		t.Errorf("User-Agent = %q; want %q", capturedHeaders.Get("User-Agent"), userAgent)
	}
	if capturedHeaders.Get("x-opencode-client") != clientHeader {
		t.Errorf("x-opencode-client = %q; want %q", capturedHeaders.Get("x-opencode-client"), clientHeader)
	}
	if capturedHeaders.Get("x-opencode-session") == "" {
		t.Error("x-opencode-session should not be empty")
	}
	if capturedHeaders.Get("x-opencode-project") == "" {
		t.Error("x-opencode-project should not be empty")
	}
	if capturedHeaders.Get("x-opencode-request") == "" {
		t.Error("x-opencode-request should not be empty")
	}
	if capturedHeaders.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q; want %q", capturedHeaders.Get("Accept"), "application/json")
	}
}

func TestHandleModelsEmptyUpstreamResponse(t *testing.T) {
	upstreamModels := []modelEntry{}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(result.Data) == 0 {
		t.Error("handleModels returned empty data - should have used fallback")
	}
	if len(result.Data) != len(fallbackFreeModels) {
		t.Errorf("got %d fallback models; want %d", len(result.Data), len(fallbackFreeModels))
	}
}

// ---------- handlePost tests ----------

func TestHandlePostAcceptsFreeModel(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model"},
		{ID: "gpt-4", Object: "model"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	body := `{"model":"hy3-free","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlePostAcceptsFreeModelWithProviderPrefix(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	body := `{"model":"zenfree/hy3-free","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestHandlePostRejectsNonFreeModel(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model"},
		{ID: "gpt-4", Object: "model"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandlePostFallbackToHardcodedFreeList(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "gpt-4", Object: "model"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	body := `{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(proxyServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
}

// ---------- forward tests ----------

func TestHandleForwardsNonModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"forwarded":true}`))
		}
	}))
	defer upstream.Close()

	p := &proxy{
		config: config{
			upstream: upstream.URL,
			quiet:    true,
		},
		client: &http.Client{Timeout: 5 * time.Second},
		logger: log.New(io.Discard, "", 0),
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handle))
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/other")
	if err != nil {
		t.Fatalf("GET /other failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestModelListResponseFormat(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model", Created: 1000, OwnedBy: "opencode"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if result.Object != "list" {
		t.Errorf("object = %q; want %q", result.Object, "list")
	}
	if len(result.Data) == 0 {
		t.Fatal("data should not be empty")
	}

	for i, raw := range result.Data {
		var entry struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("entry %d decode error: %v", i, err)
		}
		if entry.ID == "" {
			t.Errorf("entry %d has empty id", i)
		}
		if entry.Object != "model" {
			t.Errorf("entry %d object = %q; want %q", i, entry.Object, "model")
		}
	}
}

func TestConsistentFreeDetection(t *testing.T) {
	upstreamModels := []modelEntry{
		{ID: "hy3-free", Object: "model"},
		{ID: "gpt-4", Object: "model"},
		{ID: "deepseek-v4-flash-free", Object: "model"},
	}

	proxyServer, upstream := startTestProxy(t, upstreamModels)
	defer proxyServer.Close()
	defer upstream.Close()

	resp, err := http.Get(proxyServer.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	getIDs := make(map[string]bool)
	for _, raw := range result.Data {
		var entry struct{ ID string `json:"id"` }
		json.Unmarshal(raw, &entry)
		getIDs[entry.ID] = true
	}

	if !getIDs["hy3-free"] {
		t.Error("hy3-free should be in GET /models response")
	}
	if !getIDs["deepseek-v4-flash-free"] {
		t.Error("deepseek-v4-flash-free should be in GET /models response")
	}
	if getIDs["gpt-4"] {
		t.Error("gpt-4 should NOT be in GET /models response")
	}
}

// ---------- writeError ----------

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "test error message", "test_code", "model")

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q; want %q", ct, "application/json")
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if body.Error.Message != "test error message" {
		t.Errorf("message = %q; want %q", body.Error.Message, "test error message")
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q; want %q", body.Error.Type, "invalid_request_error")
	}
	if body.Error.Code != "test_code" {
		t.Errorf("code = %q; want %q", body.Error.Code, "test_code")
	}
	if body.Error.Param != "model" {
		t.Errorf("param = %q; want %q", body.Error.Param, "model")
	}
}

func TestWriteErrorNoParam(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadGateway, "upstream error", "upstream_error", "")

	resp := rec.Result()
	defer resp.Body.Close()

	var body struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if body.Error.Param != "" {
		t.Errorf("param = %q; want empty", body.Error.Param)
	}
}

// ---------- modelEntryTemplate ----------

func TestModelEntryTemplateHasRequiredFields(t *testing.T) {
	requiredFields := []string{
		"slug", "display_name", "description",
		"supported_reasoning_levels", "context_window",
		"default_reasoning_level", "visibility",
		"supported_in_api", "base_instructions",
	}
	// Create a mock entry from the template
	entry := make(map[string]interface{})
	for k, v := range modelEntryTemplate {
		entry[k] = v
	}
	entry["slug"] = "test-model"
	entry["display_name"] = "Test Model"
	entry["description"] = "A test model"

	for _, field := range requiredFields {
		if _, ok := entry[field]; !ok {
			t.Errorf("modelEntryTemplate missing required field: %s", field)
		}
	}

	// Verify supported_reasoning_levels has the expected structure
	levels, ok := entry["supported_reasoning_levels"].([]map[string]interface{})
	if !ok {
		t.Error("supported_reasoning_levels is not []map[string]interface{}")
	} else if len(levels) != 3 {
		t.Errorf("supported_reasoning_levels has %d entries; want 3", len(levels))
	}
}

// ---------- corsMiddleware ----------

func TestCORSMiddleware(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test OPTIONS request
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d; want %d", rec.Code, http.StatusOK)
	}
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q; want %q", origin, "*")
	}

	// Test GET request passes through
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("GET status = %d; want %d", rec2.Code, http.StatusOK)
	}
}
