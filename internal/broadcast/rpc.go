package broadcast

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type endpointSecret struct {
	MEVProtected bool   `json:"mevProtected,omitempty"`
	URL          string `json:"url"`
	Bearer       string `json:"bearer,omitempty"`
}
type Endpoint struct {
	ID            string     `json:"id"`
	Provider      string     `json:"providerId"`
	Network       string     `json:"network"`
	Enabled       bool       `json:"enabled"`
	Height        int64      `json:"height"`
	MEVProtected  bool       `json:"mevProtected"`
	Healthy       bool       `json:"readHealthy"`
	Checked       time.Time  `json:"checkedAt"`
	Progressed    time.Time  `json:"heightAdvancedAt"`
	Failures      int        `json:"consecutiveFailures"`
	Level         int        `json:"cooldownLevel"`
	Cooldown      *time.Time `json:"cooldownUntil,omitempty"`
	Probe         *time.Time `json:"probeUntil,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	generation    int64
	configVersion int64
	secret        endpointSecret
}
type Service struct {
	Store *Store
	cfg   Config
	http  *http.Client
	box   cipher.AEAD
}

func New(cfg Config, st *Store, key []byte) (*Service, error) {
	if len(key) != 32 {
		return nil, errors.New("provider encryption key must be 32 bytes")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	box, e := cipher.NewGCMWithRandomNonce(b)
	if e != nil {
		return nil, e
	}
	if cfg.Workers == 0 {
		cfg.Workers = 4
	}
	if cfg.TrackingHours == 0 {
		cfg.TrackingHours = 24
	}
	if cfg.Workers < 1 || cfg.Workers > 128 || cfg.TrackingHours < 1 {
		return nil, errors.New("invalid worker count or tracking duration")
	}
	return &Service{st, cfg, &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, box}, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

type transportError struct{ detail string }

func (e *transportError) Error() string { return e.detail }

func (s *Service) call(ctx context.Context, p Endpoint, method string, params any, result any) error {
	wire := bodyJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, e := http.NewRequestWithContext(ctx, "POST", p.secret.URL, bytes.NewReader(wire))
	if e != nil {
		return &transportError{"invalid endpoint URL"}
	}
	req.Header.Set("Content-Type", "application/json")
	if p.secret.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.secret.Bearer)
	}
	resp, e := s.http.Do(req)
	if e != nil {
		return &transportError{"RPC connection failed or timed out"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &transportError{fmt.Sprintf("RPC HTTP %d", resp.StatusCode)}
	}
	var envelope struct {
		ID      int             `json:"id"`
		Version string          `json:"jsonrpc"`
		Error   *rpcError       `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	const limit = 24 << 20
	data, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || len(data) > limit || json.Unmarshal(data, &envelope) != nil || envelope.ID != 1 || envelope.Version != "2.0" {
		return &transportError{"invalid RPC response"}
	}
	if envelope.Error != nil {
		msg := envelope.Error.Message
		// Hosted providers sometimes echo the request URL or authorization token.
		secrets := []string{p.secret.URL, p.secret.Bearer}
		if u, e := url.Parse(p.secret.URL); e == nil {
			for _, part := range strings.Split(u.Path, "/") {
				if len(part) > 5 {
					secrets = append(secrets, part)
				}
			}
			for _, vals := range u.Query() {
				secrets = append(secrets, vals...)
			}
		}
		for _, v := range secrets {
			if v != "" {
				msg = strings.ReplaceAll(msg, v, "[redacted]")
			}
		}
		if len(msg) > 512 {
			msg = msg[:512]
		}
		envelope.Error.Message = msg
		return envelope.Error
	}
	if len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, result) != nil {
		return &transportError{"invalid RPC result"}
	}
	return nil
}

type sendResult struct {
	state, detail   string
	endpointFailure bool
}

func classify(err error) sendResult {
	if err == nil {
		return sendResult{state: "ACCEPTED"}
	}
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		return sendResult{"UNKNOWN", err.Error(), true}
	}
	msg := strings.TrimSpace(strings.ToLower(rpc.Message))
	// Match node acknowledgements exactly. Contract revert strings can contain
	// the same words and are not evidence that the node accepted the bytes.
	switch msg {
	case "already known", "alreadyknown", "already known transaction", "known transaction",
		"alreadyprocessed", "already processed", "this transaction has already been processed",
		"transaction simulation failed: this transaction has already been processed":
		return sendResult{state: "ALREADY_KNOWN", detail: err.Error()}
	}
	if strings.HasPrefix(msg, "execution reverted") || strings.HasPrefix(msg, "transaction simulation failed:") {
		return sendResult{"REJECTED", err.Error(), false}
	}
	for _, term := range []string{"rate limit", "too many requests", "unauthorized", "forbidden", "node is unhealthy", "service unavailable", "internal error", "timed out", "timeout", "connection reset", "bad gateway", "node is behind", "invalid api key", "invalid api-key", "access denied", "quota exceeded"} {
		if strings.Contains(msg, term) {
			return sendResult{"UNKNOWN", err.Error(), true}
		}
	}
	if rpc.Code == -32603 || rpc.Code == -32601 || rpc.Code == -32005 {
		return sendResult{"UNKNOWN", err.Error(), true}
	}
	return sendResult{"REJECTED", err.Error(), false}
}

func (s *Service) send(ctx context.Context, p Endpoint, f Family, v Version) sendResult {
	if a := nativeAdapters[f]; a != nil {
		return classify(a.send(ctx, s, p, v))
	}
	method := "eth_sendRawTransaction"
	params := []any{v.Raw}
	if f == Solana {
		method = "sendTransaction"
		params = []any{v.Raw, map[string]any{"encoding": "base64", "preflightCommitment": "confirmed", "minContextSlot": v.ContextSlot, "maxRetries": 0}}
	}
	var got string
	err := s.call(ctx, p, method, params, &got)
	if err == nil && got != v.Hash {
		err = &transportError{"RPC returned unexpected transaction identifier"}
	}
	var rpc *rpcError
	if f == EVM && errors.As(err, &rpc) {
		switch rpc.Code {
		case 1000:
			return sendResult{"ALREADY_KNOWN", err.Error(), false}
		case -32002, -32004:
			return sendResult{"UNKNOWN", err.Error(), true}
		}
	}
	return classify(err)
}
