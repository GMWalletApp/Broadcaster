package broadcast

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/url"

	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

type tonAdapter struct{}

func (tonAdapter) decode(in NativeRequest) (Version, error) {
	raw, err := base64.StdEncoding.DecodeString(in.Raw)
	if err != nil {
		return Version{}, invalid("expected external message BOC in base64")
	}
	roots, err := cell.FromBOCMultiRoot(raw)
	if err != nil || len(roots) != 1 {
		return Version{}, invalid("expected single-root TON BOC")
	}
	view, err := roots[0].BeginParse()
	var msg tlb.ExternalMessage
	if err != nil || tlb.LoadFromCell(&msg, view) != nil || msg.DstAddr == nil || msg.DstAddr.IsAddrNone() || msg.Body == nil {
		return Version{}, invalid("expected one inbound external TON message")
	}
	return nativeVersion(hex.EncodeToString(msg.NormalizedHash()), base64.StdEncoding.EncodeToString(raw)), nil
}

func (tonAdapter) height(ctx context.Context, s *Service, p Endpoint) (int64, error) {
	var reply struct {
		OK     bool
		Result struct {
			Last struct {
				Seqno     *int64
				Workchain *int
			}
			Init struct {
				Root string `json:"root_hash"`
			}
		}
	}
	_, err := s.nativeHTTP(ctx, p, "/api/v2/getMasterchainInfo", nil, "", nil, &reply)
	if err != nil {
		return 0, err
	}
	r := reply.Result
	if !reply.OK || r.Init.Root != "F6OpKZKqvqeFp6CQmFomXNMfMj2EnaUSOXN+Mh+wVWk=" || r.Last.Seqno == nil || r.Last.Workchain == nil || *r.Last.Workchain != -1 {
		return 0, errNativeEvidence
	}
	return *r.Last.Seqno, nil
}

func (tonAdapter) send(ctx context.Context, s *Service, p Endpoint, v Version) error {
	var reply struct{ OK *bool }
	_, err := s.nativeHTTP(ctx, p, "/api/v2/sendBoc", nil, "application/json", bodyJSON(map[string]string{"boc": v.Raw}), &reply)
	if err != nil {
		return err
	}
	if reply.OK == nil {
		return errNativeEvidence
	}
	if !*reply.OK {
		return &rpcError{Message: "TON message rejected"}
	}
	return nil
}

func (tonAdapter) lookup(ctx context.Context, s *Service, p Endpoint, hash string) (update, error) {
	var reply struct {
		Txs []struct {
			Hash, Account, Lt string
			In                struct {
				Norm string `json:"hash_norm"`
			} `json:"in_msg"`
			Block struct {
				Seqno     *int64
				Shard     string
				Workchain *int
			} `json:"block_ref"`
			Description struct {
				Aborted *bool
				Compute struct {
					ExitCode   *int `json:"exit_code"`
					Success    *bool
					SkipReason string `json:"reason"`
				} `json:"compute_ph"`
				Action struct {
					ResultCode *int `json:"result_code"`
					Success    *bool
				} `json:"action"`
			}
		} `json:"transactions"`
	}
	q := url.Values{"msg_hash": {hash}, "direction": {"in"}, "limit": {"100"}}
	_, err := s.nativeHTTP(ctx, p, "/api/v3/transactionsByMessage", q, "", nil, &reply)
	if err != nil {
		return update{}, err
	}
	if reply.Txs == nil {
		return update{}, errNativeEvidence
	}
	for _, tx := range reply.Txs {
		norm, err := base64.StdEncoding.DecodeString(tx.In.Norm)
		if err != nil || hex.EncodeToString(norm) != hash {
			continue
		}
		if tx.Hash == "" || tx.Account == "" || tx.Lt == "" || tx.Block.Seqno == nil || tx.Block.Workchain == nil || tx.Block.Shard == "" {
			return update{}, errNativeEvidence
		}
		proof := map[string]any{"transactionHash": tx.Hash, "account": tx.Account, "lt": tx.Lt, "seqno": *tx.Block.Seqno, "workchain": *tx.Block.Workchain, "shard": tx.Block.Shard}
		d := tx.Description
		if d.Aborted == nil {
			return included(hash, "PENDING", proof), nil
		}
		reason := "TON_TRANSACTION_ABORTED"
		if *d.Aborted {
			switch {
			case d.Compute.Success != nil && !*d.Compute.Success && d.Compute.ExitCode != nil:
				reason, proof["executionDetail"] = "TON_COMPUTE_FAILED", *d.Compute.ExitCode
			case d.Compute.SkipReason != "":
				reason, proof["executionDetail"] = "TON_COMPUTE_SKIPPED", limitedDetail(d.Compute.SkipReason)
			case d.Action.Success != nil && !*d.Action.Success && d.Action.ResultCode != nil:
				reason, proof["executionDetail"] = "TON_ACTION_FAILED", *d.Action.ResultCode
			}
		}
		u := nativeIncluded(hash, !*d.Aborted, proof)
		if *d.Aborted {
			u.reason = reason
		}
		return u, nil
	}
	return nativeTracking(), nil
}
