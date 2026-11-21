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
	return res.Value[0], nil
}

func solIncluded(hash string, status *solObservation) update {
	execution := "FAILED"
	if string(status.Error) == "null" {
		execution = "SUCCESS"
	} else if len(status.Error) == 0 {
		execution = "UNKNOWN"
	}
	return included(hash, execution, status)
}

type historyCursor struct {
	Slot      uint64 `json:"slot"`
	Expected  string `json:"expectedBlockhash"`
	Signature string `json:"signature"`
}

func (s *Service) solExpiry(ctx context.Context, p Endpoint, t *Task, v Version) (update, error) {
	checking := update{state: "CHECKING_EXPIRY", reason: "VERIFYING_HISTORY", evidence: t.Evidence}
	var height uint64
	if e := s.call(ctx, p, "getBlockHeight", []any{map[string]any{"commitment": "finalized", "minContextSlot": v.ContextSlot}}, &height); e != nil || height <= v.LastValid {
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
	if json.Unmarshal(t.Evidence, &cur) != nil || cur.Signature != v.Hash || cur.Expected == "" {
		cur = historyCursor{Signature: v.Hash}
		if e := s.call(ctx, p, "getSlot", []any{map[string]string{"commitment": "finalized"}}, &cur.Slot); e != nil {
			return checking, nil
		}
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
		if cur.Expected == "" && *block.Height <= v.LastValid {
			return checking, nil
		}
		for _, sig := range block.Signatures {
			if sig == v.Hash {
				return included(sig, "UNKNOWN", map[string]any{"slot": cur.Slot, "blockhash": block.Hash}), nil
			}
		}
		if block.Hash == v.Blockhash {
			return update{state: "RESIGN_REQUIRED", reason: "BLOCKHASH_EXPIRED", evidence: bodyJSON(map[string]any{"signature": v.Hash, "blockhash": v.Blockhash, "finalizedHeight": height, "originSlot": cur.Slot})}, nil
		}
		if cur.Slot == 0 || block.Parent >= cur.Slot || block.Prev == "" || cur.Slot < v.ContextSlot && v.ContextSlot-cur.Slot > 1024 {
			return checking, nil
		}
		cur.Slot = block.Parent
		cur.Expected = block.Prev
		checking.evidence = bodyJSON(cur)
	}
	return checking, nil
}

func (s *Service) observeSolana(ctx context.Context, p Endpoint, t *Task) (update, error) {
	var active Version
	processed := false
	for _, v := range t.Versions {
		if v.Hash == t.Active {
			active = v
		}
		status, e := s.solStatus(ctx, p, v.Hash)
		if e != nil {
			return update{}, e
		}
		if status == nil {
			continue
		}
		switch status.Commitment {
		case "confirmed", "finalized":
			return solIncluded(v.Hash, status), nil
		case "processed":
			processed = true
		default:
			return update{}, errors.New("unknown confirmation status")
		}
	}
	h, e := s.height(ctx, p)
	if e != nil {
		return update{}, e
	}
	if uint64(h) > active.LastValid || t.State == "CHECKING_EXPIRY" {
		if processed {
			return update{state: "CHECKING_EXPIRY", reason: "OBSERVED_PROCESSED", evidence: t.Evidence}, nil
		}
		return s.solExpiry(ctx, p, t, active)
	}
	if processed {
		return update{state: "TRACKING", reason: "OBSERVED_PROCESSED"}, nil
	}
	var valid struct {
		Value *bool `json:"value"`
	}
	e = s.call(ctx, p, "isBlockhashValid", []any{active.Blockhash, map[string]any{"commitment": "confirmed", "minContextSlot": active.ContextSlot}}, &valid)
	if e != nil || valid.Value == nil {
		return update{}, errors.New("blockhash validity unavailable")
	}
	if !*valid.Value {
		return update{state: "TRACKING", reason: "BLOCKHASH_NOT_VISIBLE"}, nil
	}
	return update{state: "TRACKING", send: true}, nil
}

// checkWindow verifies directly comparable getLatestBlockhash metadata. An
// older signing context can legitimately have a different hash. Its supplied
// height is only a monitoring hint until finalized history proves expiry.
func (s *Service) checkWindow(ctx context.Context, v Version, network string) error {
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
		return nil
	}
	return nil
}
