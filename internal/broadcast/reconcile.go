package broadcast

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
)

// RPCs can briefly follow different forks or index a receipt at different
// times. Each observes an independent copy; only the selected view is saved.
type chainView struct {
	endpoint Endpoint
	task     Task
	next     update
	err      error
}

func (s *Service) observe(ctx context.Context, task *Task, endpoints []Endpoint) update {
	views := make([]chainView, len(endpoints))
	var workers sync.WaitGroup
	jobs := make(chan int, len(views))
	for i := range views {
		jobs <- i
	}
	close(jobs)
	for range min(len(views), 3) {
		workers.Go(func() {
			for i := range jobs {
				v := &views[i]
				v.endpoint, v.task = endpoints[i], *task
				v.task.Versions = slices.Clone(task.Versions)
				for n := range v.task.Versions {
					v.task.Versions[n].Authorizations = slices.Clone(task.Versions[n].Authorizations)
				}
				if a := nativeAdapters[task.Family]; a != nil {
					v.next, v.err = a.lookup(ctx, s, v.endpoint, v.task.Active)
				} else if task.Family == EVM {
					v.next, v.err = s.observeEVM(ctx, v.endpoint, &v.task)
				} else {
					v.next, v.err = s.observeSolana(ctx, v.endpoint, &v.task)
				}
			}
		})
	}
	workers.Wait()

	pending := update{state: task.State, reason: "NO_AVAILABLE_PROVIDER", evidence: task.Evidence}
	if len(views) != 0 {
		pending.reason = "CHAIN_VIEW_UNAVAILABLE"
	}
	var selected *chainView
	for i := range views {
		v := &views[i]
		if v.err != nil {
			continue
		}
		if selected == nil || preferView(v.next, selected.next) {
			selected = v
		}
	}
	if selected == nil {
		return pending
	}

	next := selected.next
	if task.Family == EVM && next.hasChainResult() {
		var block struct {
			Height string `json:"blockNumber"`
			Hash   string `json:"blockHash"`
		}
		if json.Unmarshal(next.evidence, &block) != nil || !s.agreeOnBlock(ctx, endpoints, selected.endpoint.ID, block.Height, block.Hash) {
			return update{state: "RESOLVING_NONCE", reason: "CHAIN_VIEW_CONFLICT"}
		}
		for _, v := range views {
			if v.err == nil && v.next.hasChainResult() && (v.next.winner != next.winner || v.next.consuming != next.consuming || differentExecutions(next, v.next)) {
				return update{state: "RESOLVING_NONCE", reason: "CHAIN_VIEW_CONFLICT"}
			}
		}
	}
	if IsNative(task.Family) && next.hasChainResult() {
		for _, v := range views {
			if v.err == nil && v.next.hasChainResult() && (differentExecutions(next, v.next) || !sameNativeLocation(v.next.evidence, next.evidence)) {
				return update{state: "TRACKING", reason: "CHAIN_VIEW_CONFLICT"}
			}
		}
	}
	if task.Family == Solana {
		for _, v := range views {
			if v.err != nil {
				continue
			}
			if solanaDisagreement(next, v.next) {
				// Restart the historical walk at a fresh root, which may now cover
				// a processed slot that used to lie beyond the proof.
				return update{state: "CHECKING_EXPIRY", reason: "CONFLICTING_HISTORY"}
			}
		}
	}
	task.Versions = selected.task.Versions
	return next
}

// Native reads may carry optional VM diagnostics. Compare only the location proof.
func sameNativeLocation(a, b json.RawMessage) bool {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	for _, field := range []string{"executionDetail", "receiptResult"} {
		delete(left, field)
		delete(right, field)
	}
	return string(bodyJSON(left)) == string(bodyJSON(right))
}

func solanaDisagreement(selected, other update) bool {
	switch selected.state {
	case "CLOSED", "CONFIRMING":
		if other.state == "RESIGN_REQUIRED" {
			return !selected.finalized
		}
		if !other.hasChainResult() || selected.finalized != other.finalized {
			return false
		}
		return selected.winner != other.winner || selected.slot != other.slot || differentExecutions(selected, other)
	case "RESIGN_REQUIRED":
		return other.processedSlot > selected.rootSlot || other.valid
	default:
		return false
	}
}

func (u update) hasChainResult() bool { return u.state == "CLOSED" || u.winner != "" }

func differentExecutions(a, b update) bool {
	return a.execution != b.execution && a.execution != "PENDING" && b.execution != "PENDING"
}

func preferView(candidate, current update) bool {
	a, b := priority(candidate), priority(current)
	if a != b {
		return a > b
	}
	if current.execution == "PENDING" && (candidate.execution == "SUCCESS" || candidate.execution == "FAILED") {
		return true
	}
	return current.reason == "EXECUTION_CAUSE_UNAVAILABLE" && candidate.execution == "FAILED" && candidate.reason != "" && candidate.reason != current.reason
}

// A positive result wins over an empty index. Nonce consumption and expiry
// checks take precedence over a request to send the same bytes again.
func priority(u update) int {
	switch {
	case u.hasChainResult() && u.finalized:
		return 10
	case u.hasChainResult():
		return 9
	case u.state == "RESIGN_REQUIRED":
		return 8
	case u.state == "RESOLVING_NONCE" || u.state == "CHECKING_EXPIRY" && u.advanced:
		return 7
	case u.state == "CHECKING_EXPIRY":
		return 6
	case u.processedSlot != 0:
		return 5
	case u.send:
		return 4
	default:
		return 1
	}
}

// All readable peers must agree on a new block at the original height
// before a previously included transaction can be scheduled again.
func (s *Service) evmInclusionOrphaned(ctx context.Context, ps []Endpoint, evidence json.RawMessage) bool {
	var old struct {
		Height string `json:"blockNumber"`
		Hash   string `json:"blockHash"`
	}
	if json.Unmarshal(evidence, &old) != nil || !isHash(old.Hash) {
		return false
	}
	for _, p := range ps {
		block, err := s.block(ctx, p, old.Height, false)
		if err == nil {
			return block.Hash != old.Hash && s.agreeOnBlock(ctx, ps, p.ID, old.Height, block.Hash)
		}
	}
	return false
}

// The witness must still see the same canonical block after any slow trace or
// history walk. A peer behind that height has no vote; a different hash does.
func (s *Service) agreeOnBlock(ctx context.Context, endpoints []Endpoint, witness, height, hash string) bool {
	for _, p := range endpoints {
		block, err := s.block(ctx, p, height, false)
		if err != nil {
			if p.ID == witness {
				return false
			}
			continue
		}
		if block.Hash != hash {
			return false
		}
	}
	return ctx.Err() == nil
}
