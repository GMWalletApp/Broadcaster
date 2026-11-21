package broadcast

import (
	"context"
	"encoding/json"
)

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
	cursorData := t.Evidence
	if t.State == "CONFIRMING" {
		var prior solanaInclusion
		if json.Unmarshal(t.Evidence, &prior) == nil {
			cursorData = prior.History
		}
	}
	var cur historyCursor
	if json.Unmarshal(cursorData, &cur) != nil || cur.Signature != v.Hash || cur.Expected == "" || cur.Root == 0 {
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
				if status := s.solExecution(ctx, p, sig, cur.Slot, "finalized"); status != nil {
					return solIncluded(sig, status), nil
				}
				u := included(sig, "PENDING", map[string]any{"slot": cur.Slot, "blockhash": block.Hash})
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
