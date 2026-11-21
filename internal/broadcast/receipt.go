package broadcast

import (
	"context"
	"encoding/json"
	"slices"
)

// completeReceipt fills in an execution result for an already observed block.
// A different block hash (or Solana slot) cannot revise the inclusion decision.
func (s *Service) completeReceipt(ctx context.Context, t *Task) {
	var original struct {
		BlockHash   string `json:"blockHash"`
		Slot        uint64 `json:"slot"`
		SolanaBlock string `json:"blockhash"`
	}
	if json.Unmarshal(t.Evidence, &original) != nil {
		return
	}
	ps, err := s.endpoints(ctx, t.Network, true)
	if err != nil {
		return
	}
	for _, p := range ps {
		var u update
		switch t.Family {
		case EVM:
			v := Version{Hash: t.Winner}
			result, found, err := s.evmLocation(ctx, p, &v)
			if err != nil || !found {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			var block struct {
				Hash string `json:"blockHash"`
			}
			if json.Unmarshal(result.evidence, &block) != nil || block.Hash != original.BlockHash {
				continue
			}
			u = result
		case Solana:
			st, err := s.solStatus(ctx, p, t.Winner)
			if err != nil || st == nil || st.Slot != original.Slot || (st.Commitment != "confirmed" && st.Commitment != "finalized") {
				continue
			}
			if original.SolanaBlock != "" {
				var block *struct {
					Hash       string   `json:"blockhash"`
					Signatures []string `json:"signatures"`
				}
				params := []any{original.Slot, map[string]any{"commitment": "finalized", "transactionDetails": "signatures", "rewards": false, "maxSupportedTransactionVersion": 0}}
				if st.Commitment != "finalized" || s.call(ctx, p, "getBlock", params, &block) != nil || block == nil || block.Hash != original.SolanaBlock || !slices.Contains(block.Signatures, t.Winner) {
					continue
				}
			}
			u = solIncluded(t.Winner, st)
		}
		if u.execution != "SUCCESS" && u.execution != "FAILED" {
			continue
		}
		// Retain the evidence and observation time that originally closed the task.
		var originalFields, details map[string]json.RawMessage
		if json.Unmarshal(t.Evidence, &originalFields) != nil || json.Unmarshal(u.evidence, &details) != nil {
			continue
		}
		for name, value := range details {
			if name != "observedAt" {
				originalFields[name] = value
			}
		}
		u.evidence = bodyJSON(originalFields)
		r, err := s.Store.db.ExecContext(ctx, "UPDATE tasks SET execution=?,evidence=?,revision=revision+1 WHERE id=? AND revision=? AND state='CLOSED' AND outcome='INCLUDED' AND execution='UNKNOWN'", u.execution, []byte(u.evidence), t.ID, t.Revision)
		if err == nil && affected(r) == 1 {
			t.Execution = u.execution
			t.Evidence = u.evidence
			t.Revision++
		}
		return
	}
}
