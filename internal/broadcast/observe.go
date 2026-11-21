package broadcast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type update struct {
	state, reason, outcome, winner, consuming, execution string
	evidence                                             json.RawMessage
	send, bump, rejected                                 bool
	finalized, valid, advanced                           bool
	slot, processedSlot, rootSlot                        uint64
}

func included(hash, execution string, proof any) update {
	u := update{state: "CLOSED", outcome: "INCLUDED", winner: hash, execution: execution, evidence: bodyJSON(proof)}
	if execution == "PENDING" {
		u.state, u.outcome, u.reason = "CONFIRMING", "", "AWAITING_EXECUTION_RESULT"
	}
	return u
}

type evmTxRef struct {
	Hash   string                       `json:"hash"`
	Block  string                       `json:"blockHash"`
	Height string                       `json:"blockNumber"`
	Index  string                       `json:"transactionIndex"`
	Sender string                       `json:"from"`
	Nonce  string                       `json:"nonce"`
	Auth   []types.SetCodeAuthorization `json:"authorizationList"`
}
type evmBlock struct {
	Hash          string            `json:"hash"`
	Height        string            `json:"number"`
	Transactions  []json.RawMessage `json:"transactions"`
	BaseFee       *hexutil.Big      `json:"baseFeePerGas"`
	ConsumerIndex uint64            `json:"-"`
}

func (s *Service) nonceAt(ctx context.Context, p Endpoint, address, tag string) (uint64, error) {
	var n string
	e := s.call(ctx, p, "eth_getTransactionCount", []any{address, tag}, &n)
	if e != nil {
		return 0, e
	}
	return hexutil.DecodeUint64(n)
}

func isHash(value string) bool {
	var hash common.Hash
	return hash.UnmarshalText([]byte(value)) == nil
}

func (s *Service) block(ctx context.Context, p Endpoint, tag string, full bool) (*evmBlock, error) {
	var b *evmBlock
	e := s.call(ctx, p, "eth_getBlockByNumber", []any{tag, full}, &b)
	if e != nil {
		return nil, e
	}
	if b == nil || !isHash(b.Hash) {
		return nil, errors.New("missing canonical block")
	}
	if _, err := hexutil.DecodeUint64(b.Height); err != nil || tag != "latest" && b.Height != tag {
		return nil, errors.New("block height mismatch")
	}
	return b, nil
}

func (s *Service) evmLocation(ctx context.Context, p Endpoint, v *Version) (update, bool, error) {
	var receipt json.RawMessage
	if e := s.call(ctx, p, "eth_getTransactionReceipt", []any{v.Hash}, &receipt); e != nil {
		return update{}, false, e
	}
	var ref *evmTxRef
	var status struct {
		Status string `json:"status"`
	}
	if string(receipt) != "null" {
		var r struct {
			Hash   string `json:"transactionHash"`
			Block  string `json:"blockHash"`
			Height string `json:"blockNumber"`
			Index  string `json:"transactionIndex"`
		}
		if json.Unmarshal(receipt, &r) != nil || r.Hash != v.Hash {
			return update{}, false, errors.New("receipt identity mismatch")
		}
		ref = &evmTxRef{Hash: r.Hash, Block: r.Block, Height: r.Height, Index: r.Index}
		_ = json.Unmarshal(receipt, &status)
	} else {
		if e := s.call(ctx, p, "eth_getTransactionByHash", []any{v.Hash}, &ref); e != nil {
			return update{}, false, e
		}
		if ref == nil {
			return update{}, false, nil
		}
		if ref.Hash != v.Hash {
			return update{}, false, errors.New("unexpected transaction")
		}
		if ref.Block == "" {
			v.State = "PENDING"
			return update{}, false, nil
		}
	}
	b, e := s.block(ctx, p, ref.Height, false)
	if e != nil {
		return update{}, false, e
	}
	idx, e := hexutil.DecodeUint64(ref.Index)
	if isHash(ref.Block) && b.Hash != ref.Block {
		// A receipt from a displaced block can outlive the fork in an RPC cache.
		return update{}, false, nil
	}
	if e != nil || idx >= uint64(len(b.Transactions)) || b.Hash != ref.Block {
		return update{}, false, errors.New("inconsistent block or position")
	}
	var hash string
	if json.Unmarshal(b.Transactions[idx], &hash) != nil || hash != v.Hash {
		return update{}, false, errors.New("canonical block mismatch")
	}
	execution := "PENDING"
	switch status.Status {
	case "0x1":
		execution = "SUCCESS"
	case "0x0":
		execution = "FAILED"
	}
	proof := map[string]any{"completionBasis": "BLOCK_INCLUSION", "blockHash": b.Hash, "blockNumber": b.Height, "transactionIndex": ref.Index, "receipt": receipt}
	reason := ""
	if execution == "FAILED" {
		reason = "EXECUTION_CAUSE_UNAVAILABLE"
		var frame struct{ Error, RevertReason string }
		params := []any{v.Hash, map[string]any{"tracer": "callTracer", "tracerConfig": map[string]bool{"onlyTopCall": true}}}
		if s.call(ctx, p, "debug_traceTransaction", params, &frame) == nil && frame.Error != "" {
			proof["vmError"] = limitedDetail(frame.Error)
			switch strings.ToLower(frame.Error) {
			case "out of gas":
				reason = "EVM_OUT_OF_GAS"
			case "execution reverted":
				reason = "EVM_REVERTED"
			default:
				reason = "EVM_VM_ERROR"
			}
			if frame.RevertReason != "" {
				proof["revertReason"] = limitedDetail(frame.RevertReason)
			}
		}
	}
	u := included(v.Hash, execution, proof)
	if reason != "" {
		u.reason = reason
	}
	return u, true, nil
}

type accountState struct {
	Nonce uint64 `json:"nonce"`
	Code  string `json:"code"`
}

func applyAuth(list []Authorization, states map[string]accountState, sender string, exact bool) {
	// Entry processing follows the outer nonce increment, and successful tuples
	// affect later tuples from the same authority within this transaction.
	if st, ok := states[sender]; ok {
		st.Nonce++
		states[sender] = st
	}
	for i := range list {
		a := &list[i]
		if a.Result == "SKIPPED" && (a.Reason == "INVALID_AUTH_SIGNATURE" || a.Reason == "AUTH_CHAIN_MISMATCH" || a.Reason == "AUTH_NONCE_OVERFLOW") {
			continue
		}
		st, ok := states[a.Authority]
		if !ok {
			a.Prediction = "UNKNOWN"
			a.Reason = "AUTHORITY_STATE_UNAVAILABLE"
			a.ObservedNonce = ""
			continue
		}
		a.ObservedNonce = strconv.FormatUint(st.Nonce, 10)
		a.Prediction = "SKIPPED"
		a.Reason = ""
		switch {
		case st.Code != "" && st.Code != "0x" && !(len(st.Code) == 48 && strings.HasPrefix(st.Code, "0xef0100")):
			a.Reason = "AUTHORITY_HAS_CODE"
		case strconv.FormatUint(st.Nonce, 10) != a.Nonce:
			a.Reason = "AUTH_NONCE_MISMATCH"
		default:
			a.Prediction = "APPLIED"
			st.Nonce++
			st.Code = "0xef0100" + strings.TrimPrefix(strings.ToLower(a.Delegate), "0x")
			if common.HexToAddress(a.Delegate) == (common.Address{}) {
				st.Code = "0x"
			}
			states[a.Authority] = st
		}
		if exact {
			a.Result = a.Prediction
		}
	}
}

func (s *Service) diagnose(ctx context.Context, p Endpoint, v *Version, sender string, block string, exact bool) {
	if len(v.Authorizations) == 0 {
		return
	}
	states := map[string]accountState{}
	if exact {
		var pre map[string]accountState
		if s.call(ctx, p, "debug_traceTransaction", []any{v.Hash, map[string]any{"tracer": "prestateTracer", "tracerConfig": map[string]bool{"includeEmpty": true}}}, &pre) != nil {
			return
		}
		for k, st := range pre {
			states[strings.ToLower(k)] = st
		}
	} else {
		for _, a := range v.Authorizations {
			if a.Authority == "" {
				continue
			}
			if _, ok := states[a.Authority]; ok {
				continue
			}
			n, e := s.nonceAt(ctx, p, a.Authority, block)
			if e != nil {
				continue
			}
			var code string
			if s.call(ctx, p, "eth_getCode", []any{a.Authority, block}, &code) == nil {
				states[a.Authority] = accountState{n, strings.ToLower(code)}
			}
		}
	}
	for i := range v.Authorizations {
		v.Authorizations[i].ObservedBlock = block
	}
	applyAuth(v.Authorizations, states, sender, exact)
}

// Locate the first canonical block after the account crossed the task nonce.
// Archival reads may be unavailable; callers then keep RESOLVING_NONCE.
func (s *Service) consumer(ctx context.Context, p Endpoint, t Task, target, hi uint64) (string, *evmBlock, error) {
	// Most conflicts are recent. Start at the tip so a node which prunes
	// historical state can still identify a transaction in its retained window.
	tip := hi
	var lo uint64
	for step := uint64(1); hi > 0; {
		probe := tip - min(tip, step)
		n, err := s.nonceAt(ctx, p, t.Sender, hexutil.EncodeUint64(probe))
		if err != nil || n <= target {
			lo = probe + 1
			break
		}
		hi = probe
		if step > tip/2 {
			step = tip
		} else {
			step *= 2
		}
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		n, e := s.nonceAt(ctx, p, t.Sender, hexutil.EncodeUint64(mid))
		if e != nil {
			// A missing state is never proof of consumption. The block itself
			// must still identify the exact sender/nonce or an applied tuple.
			lo = mid + 1
			continue
		}
		if n > target {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	b, e := s.block(ctx, p, hexutil.EncodeUint64(lo), true)
	if e != nil {
		return "", nil, e
	}
	for i, raw := range b.Transactions {
		var tx evmTxRef
		if json.Unmarshal(raw, &tx) != nil || !isHash(tx.Hash) {
			continue
		}
		if strings.EqualFold(tx.Sender, t.Sender) && tx.Nonce == hexutil.EncodeUint64(target) {
			b.ConsumerIndex = uint64(i)
			return tx.Hash, b, nil
		}
		if len(tx.Auth) == 0 {
			continue
		}
		v := Version{Hash: tx.Hash}
		for i, a := range tx.Auth {
			addr, e := a.Authority()
			v.Authorizations = append(v.Authorizations, Authorization{Index: i, Authority: strings.ToLower(addr.Hex()), Delegate: a.Address.Hex(), Nonce: strconv.FormatUint(a.Nonce, 10), Result: "UNKNOWN"})
			if e != nil || a.Nonce == math.MaxUint64 || !a.ChainID.IsZero() && a.ChainID.ToBig().String() != t.Network {
				v.Authorizations[i].Result = "SKIPPED"
				v.Authorizations[i].Reason = "INVALID_AUTH_SIGNATURE"
			}
		}
		relevant := false
		for _, a := range v.Authorizations {
			if a.Authority == t.Sender && a.Nonce == t.Nonce {
				relevant = true
			}
		}
		if relevant {
			s.diagnose(ctx, p, &v, strings.ToLower(tx.Sender), b.Hash, true)
			for _, a := range v.Authorizations {
				if a.Result == "APPLIED" && a.Authority == t.Sender && a.Nonce == t.Nonce {
					b.ConsumerIndex = uint64(i)
					return tx.Hash, b, nil
				}
			}
		}
	}
	return "", b, nil
}

func (s *Service) observeEVM(ctx context.Context, p Endpoint, t *Task) (update, error) {
	var lookupErr error
	for i := range t.Versions {
		v := &t.Versions[i]
		u, yes, e := s.evmLocation(ctx, p, v)
		if e != nil {
			lookupErr = e
			continue
		}
		if yes {
			var location struct {
				BlockHash string `json:"blockHash"`
			}
			if len(v.Authorizations) > 0 && json.Unmarshal(u.evidence, &location) == nil {
				s.diagnose(ctx, p, v, t.Sender, location.BlockHash, true)
			}
			return u, nil
		}
	}
	head, e := s.block(ctx, p, "latest", false)
	if e != nil {
		return update{}, e
	}
	current, e := s.nonceAt(ctx, p, t.Sender, head.Height)
	if e != nil {
		return update{}, e
	}
	wanted, _ := strconv.ParseUint(t.Nonce, 10, 64)
	if current > wanted {
		hi, e := hexutil.DecodeUint64(head.Height)
		if e != nil {
			return update{}, e
		}
		h, b, e := s.consumer(ctx, p, *t, wanted, hi)
		if e != nil || h == "" {
			return update{state: "RESOLVING_NONCE", reason: "CONSUMER_UNKNOWN"}, nil
		}
		for i := range t.Versions {
			if t.Versions[i].Hash == h {
				return included(h, "PENDING", map[string]any{"blockHash": b.Hash, "blockNumber": b.Height, "completionBasis": "BLOCK_INCLUSION", "transactionIndex": hexutil.EncodeUint64(b.ConsumerIndex)}), nil
			}
		}
		return update{state: "CLOSED", outcome: "REPLACED_EXTERNALLY", reason: "NONCE_CONSUMED_EXTERNALLY", consuming: h, evidence: bodyJSON(map[string]string{"blockHash": b.Hash, "blockNumber": b.Height, "transactionIndex": hexutil.EncodeUint64(b.ConsumerIndex)})}, nil
	}
	if lookupErr != nil {
		return update{}, lookupErr
	}
	if current < wanted && !t.Force {
		pending, e := s.nonceAt(ctx, p, t.Sender, "pending")
		if e != nil {
			return update{}, e
		}
		if pending < wanted {
			return update{state: "QUEUED", reason: "NONCE_GAP"}, nil
		}
	}
	for i := range t.Versions {
		v := &t.Versions[i]
		if v.Hash == t.Active {
			s.diagnose(ctx, p, v, t.Sender, head.Height, false)
			// A suggested gas price includes a discretionary tip. Only the base fee
			// proves that the signed fee cap is currently too low for inclusion.
			if !t.Force && head.BaseFee != nil && time.Since(t.Created) > 2*time.Minute {
				var signed types.Transaction
				raw, err := hexutil.Decode(v.Raw)
				if err == nil && signed.UnmarshalBinary(raw) == nil && signed.GasFeeCap().Cmp(head.BaseFee.ToInt()) < 0 {
					return update{state: "TRACKING", reason: "REPLACEMENT_REQUIRED", bump: true}, nil
				}
			}
		}
	}
	return update{state: "TRACKING", send: true}, nil
}
