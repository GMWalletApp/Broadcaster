package broadcast

import (
	"context"
	"encoding/json"
	"errors"
)

type solObservation struct {
	Slot       uint64          `json:"slot"`
	Commitment string          `json:"confirmationStatus"`
	Error      json.RawMessage `json:"err"`
}

func (s *Service) solStatus(ctx context.Context, p Endpoint, sig string) (*solObservation, error) {
	var res struct {
		Value []*solObservation `json:"value"`
	}
	err := s.call(ctx, p, "getSignatureStatuses", []any{[]string{sig}, map[string]bool{"searchTransactionHistory": true}}, &res)
	if err != nil {
		return nil, err
	}
	if len(res.Value) != 1 {
		return nil, errors.New("invalid signature status result")
	}
	st := res.Value[0]
	if st != nil && (st.Slot == 0 || st.Commitment != "processed" && st.Commitment != "confirmed" && st.Commitment != "finalized") {
		return nil, errors.New("invalid signature status or slot")
	}
	return st, nil
}

func solIncluded(hash string, status *solObservation) update {
	execution := "FAILED"
	if string(status.Error) == "null" {
		execution = "SUCCESS"
	} else if len(status.Error) == 0 {
		execution = "UNKNOWN"
	}
	u := included(hash, execution, status)
	u.finalized = status.Commitment == "finalized"
	u.slot = status.Slot
	return u
}

type historyCursor struct {
	Slot      uint64 `json:"slot"`
	Expected  string `json:"expectedBlockhash"`
	Signature string `json:"signature"`
	Root      uint64 `json:"rootSlot"`
}

// A submission accepted during an outage may carry an inflated expiry. A fresh
// finalized blockhash gives an independent upper bound for an older context.
func (s *Service) impossibleExpiry(ctx context.Context, p Endpoint, v Version) bool {
	var head struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Hash      string `json:"blockhash"`
			LastValid uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	params := []any{map[string]any{"commitment": "finalized", "minContextSlot": v.ContextSlot}}
	return s.call(ctx, p, "getLatestBlockhash", params, &head) == nil && head.Context.Slot >= v.ContextSlot && head.Value.Hash != "" && head.Value.LastValid > 0 && head.Value.LastValid < v.LastValid
}

func (s *Service) solExpiry(ctx context.Context, p Endpoint, t *Task, v Version) (update, error) {
	checking := update{state: "CHECKING_EXPIRY", reason: "VERIFYING_HISTORY", evidence: t.Evidence}
	var height uint64
	if e := s.call(ctx, p, "getBlockHeight", []any{map[string]any{"commitment": "finalized", "minContextSlot": v.ContextSlot}}, &height); e != nil {
		return checking, nil
	}
	inflated := height <= v.LastValid
	if inflated && !s.impossibleExpiry(ctx, p, v) {
		return checking, nil
	}
	var valid struct {
		Value *bool `json:"value"`
	}
	if e := s.call(ctx, p, "isBlockhashValid", []any{v.Blockhash, map[string]any{"commitment": "finalized", "minContextSlot": v.ContextSlot}}, &valid); e != nil || valid.Value == nil || *valid.Value {
		return checking, nil
	}
	// Walk the finalized parent chain, including skipped slots, down to the
	// actual signed blockhash. A null status alone cannot prove non-inclusion.
	var cur historyCursor
	if json.Unmarshal(t.Evidence, &cur) != nil || cur.Signature != v.Hash || cur.Expected == "" || cur.Root == 0 {
		cur = historyCursor{Signature: v.Hash}
		if e := s.call(ctx, p, "getSlot", []any{map[string]any{"commitment": "finalized", "minContextSlot": v.ContextSlot}}, &cur.Slot); e != nil || cur.Slot < v.ContextSlot {
			return checking, nil
		}
		cur.Root = cur.Slot
	}
	for range 12 {
		var block *struct {
			Hash       string   `json:"blockhash"`
			Prev       string   `json:"previousBlockhash"`
			Parent     uint64   `json:"parentSlot"`
			Height     *uint64  `json:"blockHeight"`
			Signatures []string `json:"signatures"`
		}
		e := s.call(ctx, p, "getBlock", []any{cur.Slot, map[string]any{"transactionDetails": "signatures", "rewards": false, "commitment": "finalized", "maxSupportedTransactionVersion": 0}}, &block)
		if e != nil || block == nil || block.Hash == "" || block.Signatures == nil || block.Height == nil || cur.Expected != "" && block.Hash != cur.Expected {
			return checking, nil
		}
		if cur.Expected == "" && (!inflated && *block.Height <= v.LastValid || inflated && *block.Height < height) {
			return checking, nil
		}
		for _, sig := range block.Signatures {
			if sig == v.Hash {
				u := included(sig, "UNKNOWN", map[string]any{"slot": cur.Slot, "blockhash": block.Hash})
				u.finalized, u.slot = true, cur.Slot
				return u, nil
			}
		}
		if block.Hash == v.Blockhash || v.OriginSlot > 0 && cur.Slot <= v.OriginSlot {
			return update{state: "RESIGN_REQUIRED", reason: "BLOCKHASH_EXPIRED", rootSlot: cur.Root, evidence: bodyJSON(map[string]any{"signature": v.Hash, "blockhash": v.Blockhash, "finalizedHeight": height, "historyFloorSlot": cur.Slot, "rootSlot": cur.Root, "verifiedOriginSlot": v.OriginSlot})}, nil
		}
		if cur.Slot == 0 || block.Parent >= cur.Slot || block.Prev == "" || v.OriginSlot == 0 && cur.Slot < v.ContextSlot && v.ContextSlot-cur.Slot > 1024 {
			checking.reason = "BLOCKHASH_ORIGIN_UNVERIFIED"
			return checking, nil
		}
		cur.Slot = block.Parent
		cur.Expected = block.Prev
		checking.evidence = bodyJSON(cur)
		checking.advanced = true
	}
	return checking, nil
}

func (s *Service) observeSolana(ctx context.Context, p Endpoint, t *Task) (update, error) {
	var active *Version
	var processedSlot uint64
	var lookupErr error
	for i := range t.Versions {
		v := &t.Versions[i]
		if v.Hash == t.Active {
			active = v
		}
		status, e := s.solStatus(ctx, p, v.Hash)
		if e != nil {
			if v.State == "EXPIRED" {
				continue
			}
			lookupErr = e
			continue
		}
		if status == nil {
			continue
		}
		switch status.Commitment {
		case "confirmed", "finalized":
			return solIncluded(v.Hash, status), nil
		case "processed":
			processedSlot = max(processedSlot, status.Slot)
		default:
			return update{}, errors.New("unknown confirmation status")
		}
	}
	observed := update{state: "TRACKING", reason: "OBSERVED_PROCESSED", processedSlot: processedSlot}
	if lookupErr != nil && processedSlot == 0 {
		return update{}, lookupErr
	}
	if active == nil {
		return update{}, errors.New("missing active Solana version")
	}
	h, e := s.height(ctx, p)
	if e != nil {
		if processedSlot > 0 {
			return observed, nil
		}
		return update{}, e
	}
	var valid struct {
		Value *bool `json:"value"`
	}
	e = s.call(ctx, p, "isBlockhashValid", []any{active.Blockhash, map[string]any{"commitment": "confirmed", "minContextSlot": active.ContextSlot}}, &valid)
	if e != nil || valid.Value == nil {
		if processedSlot > 0 {
			return observed, nil
		}
		return update{}, errors.New("blockhash validity unavailable")
	}
	if *valid.Value && active.OriginSlot == 0 {
		s.verifyOrigin(ctx, p, active)
	}
	if !*valid.Value && (uint64(h) > active.LastValid || s.impossibleExpiry(ctx, p, *active)) {
		u, err := s.solExpiry(ctx, p, t, *active)
		if u.state == "RESIGN_REQUIRED" && processedSlot > u.rootSlot {
			return update{state: "CHECKING_EXPIRY", reason: "OBSERVED_PROCESSED", processedSlot: processedSlot}, nil
		}
		u.processedSlot = processedSlot
		return u, err
	}
	if processedSlot > 0 {
		return update{state: "TRACKING", reason: "OBSERVED_PROCESSED", processedSlot: processedSlot, valid: *valid.Value}, nil
	}
	if !*valid.Value {
		if t.State == "CHECKING_EXPIRY" {
			return update{state: t.State, reason: "VERIFYING_HISTORY", evidence: t.Evidence}, nil
		}
		return update{state: "TRACKING", reason: "BLOCKHASH_NOT_VISIBLE"}, nil
	}
	return update{state: "TRACKING", send: true, valid: true}, nil
}

// checkWindow verifies directly comparable getLatestBlockhash metadata. An
// older signing context can legitimately have a different hash. Its supplied
// height is only a monitoring hint until finalized history proves expiry.
func (s *Service) checkWindow(ctx context.Context, v *Version, network string) error {
	ps, err := s.endpoints(ctx, network, true)
	if err != nil {
		return err
	}
	for _, p := range ps {
		var head struct {
			Context struct {
				Slot uint64 `json:"slot"`
			} `json:"context"`
			Value struct {
				Hash      string `json:"blockhash"`
				LastValid uint64 `json:"lastValidBlockHeight"`
			} `json:"value"`
		}
		if err = s.call(ctx, p, "getLatestBlockhash", []any{map[string]any{"commitment": "confirmed", "minContextSlot": v.ContextSlot}}, &head); err != nil || head.Context.Slot < v.ContextSlot || head.Value.Hash == "" {
			continue
		}
		if head.Value.Hash == v.Blockhash && head.Value.LastValid != v.LastValid {
			return invalid("lastValidBlockHeight does not match signed blockhash")
		}
		if head.Value.LastValid < v.LastValid {
			return invalid("expiry exceeds the RPC validity window")
		}
		if s.verifyOrigin(ctx, p, v) {
			return nil
		}
	}
	return nil
}

// The signed blockhash in an actual block binds its creation slot. A caller's
// context slot by itself cannot bound a proof of absence on the finalized fork.
func (s *Service) verifyOrigin(ctx context.Context, p Endpoint, v *Version) bool {
	var origin *struct {
		Hash string `json:"blockhash"`
	}
	params := []any{v.ContextSlot, map[string]any{"commitment": "confirmed", "transactionDetails": "none", "rewards": false, "maxSupportedTransactionVersion": 0}}
	if s.call(ctx, p, "getBlock", params, &origin) != nil || origin == nil || origin.Hash != v.Blockhash {
		return false
	}
	v.OriginSlot = v.ContextSlot
	return true
}
