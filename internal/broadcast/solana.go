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
		execution = "PENDING"
	}
	u := included(hash, execution, status)
	if execution == "FAILED" {
		u.reason = solanaFailure(status.Error)
	}
	u.finalized = status.Commitment == "finalized"
	u.slot = status.Slot
	return u
}

// Finalized block history may keep the signature after the status index is pruned.
func (s *Service) solExecution(ctx context.Context, p Endpoint, hash string, slot uint64, commitment string) *solObservation {
	var tx *struct {
		Slot        uint64
		Transaction struct{ Signatures []string }
		Meta        *struct {
			Error json.RawMessage `json:"err"`
		}
	}
	params := []any{hash, map[string]any{"commitment": commitment, "encoding": "json", "maxSupportedTransactionVersion": 0}}
	if s.call(ctx, p, "getTransaction", params, &tx) != nil || tx == nil || tx.Slot != slot || len(tx.Transaction.Signatures) == 0 || tx.Transaction.Signatures[0] != hash || tx.Meta == nil || len(tx.Meta.Error) == 0 {
		return nil
	}
	return &solObservation{Slot: slot, Commitment: commitment, Error: tx.Meta.Error}
}

type solanaInclusion struct {
	Slot       uint64          `json:"slot"`
	Commitment string          `json:"confirmationStatus"`
	Blockhash  string          `json:"blockhash"`
	History    json.RawMessage `json:"historyCursor"`
}

func (p solanaInclusion) finalized() bool { return p.Commitment == "finalized" || p.Blockhash != "" }

// Preserve the inclusion observation while a multi-round scan checks whether
// a confirmed fork was displaced. Only a complete finalized exclusion proof
// covering the previously observed slot can release the payment for signing.
func reconcileSolanaConfirmation(t *Task, next update) update {
	pending := update{state: t.State, reason: t.Reason, winner: t.Winner, execution: t.Execution, evidence: t.Evidence}
	var prior solanaInclusion
	if json.Unmarshal(t.Evidence, &prior) != nil || prior.finalized() || prior.Slot == 0 || t.Winner != t.Active {
		return pending
	}
	if next.state == "RESIGN_REQUIRED" && next.rootSlot >= prior.Slot {
		return next
	}
	if next.state == "CHECKING_EXPIRY" || next.state == "RESIGN_REQUIRED" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(t.Evidence, &fields) != nil || fields == nil {
			return pending
		}
		switch {
		case next.advanced:
			fields["historyCursor"] = next.evidence
		case next.state == "RESIGN_REQUIRED" || next.reason == "CONFLICTING_HISTORY":
			delete(fields, "historyCursor")
		}
		pending.evidence = bodyJSON(fields)
	}
	return pending
}

func (s *Service) observeSolana(ctx context.Context, p Endpoint, t *Task) (update, error) {
	if t.State == "CONFIRMING" {
		var proof solanaInclusion
		if json.Unmarshal(t.Evidence, &proof) == nil && proof.Slot > 0 {
			if status := s.solExecution(ctx, p, t.Winner, proof.Slot, "finalized"); status != nil {
				return solIncluded(t.Winner, status), nil
			}
		}
		if proof.finalized() && proof.Slot > 0 {
			if status, err := s.solStatus(ctx, p, t.Winner); err == nil && status != nil && status.Slot == proof.Slot && status.Commitment == "finalized" && len(status.Error) > 0 {
				return solIncluded(t.Winner, status), nil
			}
			return update{}, errors.New("finalized execution metadata unavailable")
		}
	}
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
			if len(status.Error) == 0 {
				if full := s.solExecution(ctx, p, v.Hash, status.Slot, status.Commitment); full != nil {
					status = full
				}
			}
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
