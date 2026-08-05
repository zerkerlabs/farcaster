// Command claude-agent turns a local Claude Code install into a gateway
// upstream: a persistent HTTP service the Zerker gateway can catalog, proxy,
// meter, and price like any other agent.
//
// This is the reference answer to "how do I upgrade my coding agent into a
// permanent, sellable agent?" — wrap it in an HTTP surface, give it a
// specialty via AGENT_SPECIALTY, put it on a public URL, and register that
// URL in the gateway catalog. From there the gateway owns auth, policy,
// payment gating (x402), and invocation capture; this process owns nothing
// but the model call.
//
//	AGENT_SPECIALTY="You are the payments-team expert. Answer only from the
//	  perspective of our x402 integration." ./claude-agent
//
//	curl -s localhost:9200/invoke -d '{"prompt":"What does a 402 mean here?"}'
//
// It shells out to `claude -p` (headless mode) per request, so it uses
// whatever Claude Code auth the host already has. A production deployment
// would swap the exec for a long-running Claude Agent SDK service; the HTTP
// contract stays the same, which is the point — the gateway neither knows
// nor cares what is behind the URL.
//
// SECURITY: this executes caller-supplied prompts with the host's Claude
// credentials. Never expose it directly to the internet — its only intended
// caller is the gateway's proxy, which is where auth, tenancy, rate limits,
// and payment sit. Run it on a private network with only the gateway (or a
// tunnel the gateway points at) able to reach it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// requestTimeout bounds a single model call. Headless claude runs can be
// slow on multi-turn work; transactional gateway invocations poll for the
// result, so a generous bound is fine.
const requestTimeout = 120 * time.Second

// maxConcurrent bounds simultaneous claude processes — each one is a full
// model session, not a cheap handler. Excess requests queue on the channel.
const maxConcurrent = 2

// maxPromptBytes caps the request body; the gateway caps its own request
// bodies too, but this process must not trust its caller for its limits.
const maxPromptBytes = 1 << 20 // 1 MiB

type invokeRequest struct {
	Prompt string `json:"prompt"`
}

// claudeResult is the subset of `claude -p --output-format json` output the
// wrapper passes through. Cost and duration make gateway-side pricing
// decisions legible: you can see what a call costs you before pricing what
// it costs your callers.
type claudeResult struct {
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	IsError    bool    `json:"is_error"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	claudeBin := envOr("CLAUDE_BIN", "claude")
	if _, err := exec.LookPath(claudeBin); err != nil {
		logger.Error("claude binary not found; install Claude Code or set CLAUDE_BIN", "bin", claudeBin)
		os.Exit(1)
	}
	specialty := os.Getenv("AGENT_SPECIALTY")
	addr := envOr("ADDR", ":9200")

	sem := make(chan struct{}, maxConcurrent)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /invoke", func(w http.ResponseWriter, r *http.Request) {
		var req invokeRequest
		body := http.MaxBytesReader(w, r.Body, maxPromptBytes)
		if err := json.NewDecoder(body).Decode(&req); err != nil || req.Prompt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `body must be {"prompt": "..."}`})
			return
		}

		sem <- struct{}{}
		defer func() { <-sem }()

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()

		args := []string{"-p", req.Prompt, "--output-format", "json"}
		if specialty != "" {
			args = append(args, "--append-system-prompt", specialty)
		}
		out, err := exec.CommandContext(ctx, claudeBin, args...).Output()
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			logger.Error("claude invocation failed", "err", err)
			writeJSON(w, status, map[string]string{"error": "agent invocation failed"})
			return
		}

		var res claudeResult
		if err := json.Unmarshal(out, &res); err != nil {
			logger.Error("unparseable claude output", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "agent returned unparseable output"})
			return
		}
		if res.IsError {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "agent run errored", "detail": res.Result})
			return
		}
		logger.Info("invocation served", "duration_ms", res.DurationMS, "cost_usd", res.CostUSD)
		writeJSON(w, http.StatusOK, res)
	})

	logger.Info("claude-agent upstream listening", "addr", addr, "specialized", specialty != "")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
