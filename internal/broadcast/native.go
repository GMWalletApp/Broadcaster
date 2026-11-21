package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	Bitcoin Family = "bitcoin"
	Tron    Family = "tron"
	Aptos   Family = "aptos"
	Sui     Family = "sui"
	TON     Family = "ton"
)

// The task key is derived from the submitted envelope. The hash returned by
// a write-only node is never sufficient evidence of inclusion.
type NativeRequest struct {
	Raw        string   `json:"rawTransaction"`
	Signatures []string `json:"signatures,omitempty"`
}

type nativeAdapter interface {
	decode(NativeRequest) (Version, error)
	height(context.Context, *Service, Endpoint) (int64, error)
	send(context.Context, *Service, Endpoint, Version) error
	lookup(context.Context, *Service, Endpoint, string) (update, error)
}

var nativeAdapters = map[Family]nativeAdapter{
	Bitcoin: bitcoinAdapter{}, Tron: tronAdapter{}, TON: tonAdapter{}, Aptos: aptosAdapter{}, Sui: suiAdapter{},
}

func IsNative(f Family) bool { return nativeAdapters[f] != nil }

var errNativeEvidence = errors.New("incomplete or mismatched native chain response")

func nativeTracking() update { return update{state: "TRACKING", send: true} }

func nativeIncluded(hash string, success bool, evidence any) update {
	execution := "SUCCESS"
	if !success {
		execution = "FAILED"
	}
	u := included(hash, execution, evidence)
	if !success {
		u.reason = "EXECUTION_CAUSE_UNAVAILABLE"
	}
	return u
}

func nativeVersion(hash, raw string) Version { return Version{Hash: hash, Raw: raw, State: "READY"} }

func (s *Service) nativeTransactions(w http.ResponseWriter, r *http.Request, c Client) error {
	network := r.PathValue("network")
	family := s.cfg.Networks[network].Family
	adapter := nativeAdapters[family]
	if adapter == nil {
		return invalid("unknown native network")
	}
	if r.Method == http.MethodGet {
		hash := r.PathValue("hash")
		t, err := s.Store.lookup(r.Context(), c.Name, family, network, hash)
		if err != nil {
			return err
		}
		writeJSON(w, 200, struct {
			Task
			Queried string `json:"queriedHash"`
		}{t, hash})
		return nil
	}
	var in NativeRequest
	if err := parse(r, &in); err != nil {
		return err
	}
	v, err := decodeNative(family, in)
	if err != nil {
		return err
	}
	t, created, err := s.Store.submit(r.Context(), Task{Family: family, Network: network, Owner: c.Name, State: "QUEUED"}, v)
	if err == nil {
		receipt(w, t, v.Hash, created)
	}
	return err
}

func decodeNative(f Family, in NativeRequest) (Version, error) {
	if in.Raw == "" || (f != Sui && len(in.Signatures) > 0) {
		return Version{}, invalid("expected native signed transaction")
	}
	return nativeAdapters[f].decode(in)
}

// Resolve method paths relative to a provider's API root, preserving proxy
// prefixes and private query credentials. Errors never include the URL/body.
func (s *Service) nativeHTTP(ctx context.Context, p Endpoint, path string, query url.Values, contentType string, body []byte, result any) (int, error) {
	u, err := url.Parse(p.secret.URL)
	if err != nil {
		return 0, errNativeEvidence
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	q := u.Query()
	for k, vs := range query {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	method := "GET"
	if body != nil {
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, errNativeEvidence
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if p.secret.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.secret.Bearer)
	}
	res, err := s.http.Do(req)
	if err != nil {
		return 0, &transportError{"native RPC connection failed"}
	}
	defer res.Body.Close()
	if method == "POST" && (res.StatusCode == 400 || res.StatusCode == 422) {
		return res.StatusCode, &rpcError{Message: fmt.Sprintf("native RPC rejected request (HTTP %d)", res.StatusCode)}
	}
	if res.StatusCode == http.StatusNotFound && p.Network == "aptos" && method == "GET" {
		var body struct {
			Code string `json:"error_code"`
		}
		if json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body) == nil && body.Code == "transaction_not_found" {
			return res.StatusCode, nil
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, &transportError{fmt.Sprintf("native RPC HTTP %d", res.StatusCode)}
	}
	const limit = 16 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(data) > limit || json.Unmarshal(data, result) != nil {
		return res.StatusCode, errNativeEvidence
	}
	return res.StatusCode, nil
}
