package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	upstreamURL = "https://opencode.ai/zen/v1"
	modelsDev   = "https://models.dev/api.json"
	ua          = "opencode/latest/1.17.15/cli"
)

var fallback = []string{
	"big-pickle",
	"deepseek-v4-flash-free",
	"laguna-s-2.1-free",
	"mimo-v2.5-free",
	"nemotron-3-ultra-free",
	"north-mini-code-free",
}

var hopByHop = map[string]bool{
	"authorization": true, "content-length": true, "connection": true,
	"transfer-encoding": true, "trailer": true, "host": true,
	"accept-encoding": true,
}

// ---- cost cache ----

type modelCost struct{ input float64; status string }

var (
	mu       sync.Mutex
	costs    map[string]modelCost
	costTime time.Time
)

func loadCosts(c *http.Client) {
	mu.Lock()
	defer mu.Unlock()
	if time.Now().Before(costTime) && costs != nil {
		return
	}
	resp, err := c.Get(modelsDev)
	if err != nil {
		log.Printf("models.dev: %v", err)
		return
	}
	defer resp.Body.Close()
	var raw struct {
		OpenCode struct {
			Models map[string]struct {
				Status string  `json:"status"`
				Cost   struct{ Input float64 `json:"input"` } `json:"cost"`
			} `json:"models"`
		} `json:"opencode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return
	}
	costs = make(map[string]modelCost, len(raw.OpenCode.Models))
	for id, v := range raw.OpenCode.Models {
		costs[id] = modelCost{input: v.Cost.Input, status: v.Status}
	}
	costTime = time.Now().Add(5 * time.Minute)
	log.Printf("cost cache: %d models", len(costs))
}

func isFree(id string) bool {
	mu.Lock()
	defer mu.Unlock()
	if c, ok := costs[id]; ok {
		return c.input == 0 && c.status != "deprecated"
	}
	return strings.HasSuffix(id, "-free")
}

// ---- helpers ----

func rndHex(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func randSleep(maxMs int) {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(maxMs)))
	time.Sleep(time.Duration(n.Int64()) * time.Millisecond)
}

func ocHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", ua)
	h.Set("x-opencode-client", "cli")
	h.Set("x-opencode-session", rndHex(26))
	h.Set("x-opencode-project", rndHex(26))
	h.Set("x-opencode-request", rndHex(26))
	return h
}

func jsonErr(w http.ResponseWriter, code int, msg, typ, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	e := map[string]any{"error": map[string]any{"message": msg, "type": typ}}
	if param != "" {
		e["error"].(map[string]any)["param"] = param
	}
	json.NewEncoder(w).Encode(e)
}

// ---- proxy ----

func proxy(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	randSleep(51)

	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	} else if r.Body != nil {
		rdr = r.Body
	}

	target := upstreamURL + "/" + strings.TrimPrefix(path, "/")
	req, _ := http.NewRequest(r.Method, target, rdr)

	// Copy non-hop headers
	for k, vv := range r.Header {
		if !hopByHop[strings.ToLower(k)] {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
	}
	// Overlay opencode identity
	oc := ocHeaders()
	req.Header.Set("User-Agent", oc.Get("User-Agent"))
	for _, k := range []string{"x-opencode-client", "x-opencode-session", "x-opencode-project", "x-opencode-request"} {
		req.Header.Set(k, oc.Get(k))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("upstream [%s]: %v", path, err)
		jsonErr(w, 502, "upstream error: "+err.Error(), "upstream_error", "")
		return
	}
	defer resp.Body.Close()

	// Forward response headers
	for k, vv := range resp.Header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "content-length" || lk == "transfer-encoding" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ---- handlers ----

func serveModels(w http.ResponseWriter, r *http.Request, client *http.Client) {
	loadCosts(client)
	req, _ := http.NewRequest("GET", upstreamURL+"/models", nil)
	oc := ocHeaders()
	for _, k := range []string{"User-Agent", "x-opencode-client", "x-opencode-session", "x-opencode-project", "x-opencode-request"} {
		req.Header.Set(k, oc.Get(k))
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("models upstream: %v", err)
		serveFallback(w)
		return
	}
	defer resp.Body.Close()
	var raw struct {
		Data []struct{ ID string `json:"id"` } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&raw)
	free := make([]map[string]any, 0)
	now := time.Now().Unix()
	for _, m := range raw.Data {
		if isFree(m.ID) {
			free = append(free, map[string]any{"id": m.ID, "object": "model", "created": now, "owned_by": "opencode"})
		}
	}
	if len(free) == 0 {
		serveFallback(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": free})
}

func serveFallback(w http.ResponseWriter) {
	now := time.Now().Unix()
	data := make([]map[string]any, len(fallback))
	for i, id := range fallback {
		data[i] = map[string]any{"id": id, "object": "model", "created": now, "owned_by": "opencode"}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func serveChat(w http.ResponseWriter, r *http.Request, client *http.Client) {
	loadCosts(client)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		jsonErr(w, 400, "cannot read body", "invalid_request_error", "")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		jsonErr(w, 400, "invalid JSON", "invalid_request_error", "")
		return
	}
	rawModel, _ := body["model"].(string)
	model := rawModel
	if i := strings.IndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if model != "" {
		free := make([]string, 0)
		req, _ := http.NewRequest("GET", upstreamURL+"/models", nil)
		oc := ocHeaders()
		for _, k := range []string{"User-Agent", "x-opencode-client", "x-opencode-session", "x-opencode-project", "x-opencode-request"} {
			req.Header.Set(k, oc.Get(k))
		}
		resp, err := client.Do(req)
		if err == nil {
			var raw struct {
				Data []struct{ ID string `json:"id"` } `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&raw)
			resp.Body.Close()
			for _, m := range raw.Data {
				if isFree(m.ID) {
					free = append(free, m.ID)
				}
			}
		}
		if len(free) == 0 {
			free = fallback
		}
		found := false
		for _, id := range free {
			if id == model {
				found = true
				break
			}
		}
		if !found {
			jsonErr(w, 400, fmt.Sprintf("model '%s' is not free", rawModel), "model_not_available", "model")
			return
		}
		if rawModel != model {
			body["model"] = model
			raw, _ = json.Marshal(body)
		}
	}
	proxy(w, r, "chat/completions", raw)
}

func main() {
	port := flag.Int("port", 8787, "listen port")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	log.Printf("zen-free-proxy on :%d -> %s", *port, upstreamURL)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if path == "v1" || path == "" {
			path = ""
		}
		log.Printf("%s %s ua=%s", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
		switch {
		case r.Method == "GET" && path == "models":
			serveModels(w, r, client)
		case r.Method == "POST" && path == "chat/completions":
			serveChat(w, r, client)
		default:
			proxy(w, r, path, nil)
		}
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil))
}
