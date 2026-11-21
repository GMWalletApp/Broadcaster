package broadcast

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	tronpb "github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"
)

type tronAdapter struct{}

func (tronAdapter) decode(in NativeRequest) (Version, error) {
	raw, err := hex.DecodeString(in.Raw)
	var tx tronpb.Transaction
	if err != nil || proto.Unmarshal(raw, &tx) != nil || len(tx.GetRawData().GetContract()) == 0 || len(tx.Signature) == 0 {
		return Version{}, invalid("expected signed TRON protobuf transaction in hex")
	}
	for _, sig := range tx.Signature {
		if len(sig) != 65 {
			return Version{}, invalid("invalid TRON signature length")
		}
	}
	data, err := proto.Marshal(tx.RawData)
	if err != nil {
		return Version{}, invalid("invalid TRON raw_data")
	}
	sum := sha256.Sum256(data)
	return nativeVersion(hex.EncodeToString(sum[:]), hex.EncodeToString(raw)), nil
}

type tronTxRef struct {
	ID      string `json:"txID"`
	Results []struct {
		Status string `json:"contractRet"`
	} `json:"ret"`
}

type tronBlock struct {
	ID     string `json:"blockID"`
	Header struct {
		Data struct {
			Number *int64 `json:"number"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Transactions []tronTxRef `json:"transactions"`
}

func (tronAdapter) height(ctx context.Context, s *Service, p Endpoint) (int64, error) {
	var genesis, head tronBlock
	if _, err := s.nativeHTTP(ctx, p, "/wallet/getblockbynum", nil, "application/json", []byte(`{"num":0}`), &genesis); err != nil {
		return 0, err
	}
	if genesis.ID != "00000000000000001ebf88508a03865c71d452e25f4d51194196a1d22b6653dc" {
		return 0, errNativeEvidence
	}
	if _, err := s.nativeHTTP(ctx, p, "/wallet/getnowblock", nil, "application/json", []byte("{}"), &head); err != nil {
		return 0, err
	}
	if head.Header.Data.Number == nil {
		return 0, errNativeEvidence
	}
	return *head.Header.Data.Number, nil
}

func (tronAdapter) send(ctx context.Context, s *Service, p Endpoint, v Version) error {
	var reply struct {
		Result              *bool
		Code, Txid, Message string
	}
	_, err := s.nativeHTTP(ctx, p, "/wallet/broadcasthex", nil, "application/json", bodyJSON(map[string]string{"transaction": v.Raw}), &reply)
	if err != nil {
		return err
	}
	if reply.Code == "DUP_TRANSACTION_ERROR" {
		return &rpcError{Message: "already known"}
	}
	if (reply.Result == nil || !*reply.Result) && reply.Code != "" {
		switch reply.Code {
		case "SERVER_BUSY", "NO_CONNECTION", "NOT_ENOUGH_EFFECTIVE_CONNECTION":
			return &transportError{"TRON node unavailable: " + reply.Code}
		}
		return &rpcError{Message: tronRejection(reply.Code, reply.Message)}
	}
	if reply.Result == nil {
		return errNativeEvidence
	}
	if !*reply.Result {
		return &rpcError{Message: "TRON broadcast rejected"}
	}
	if reply.Txid != v.Hash {
		return errNativeEvidence
	}
	return nil
}

func (tronAdapter) lookup(ctx context.Context, s *Service, p Endpoint, hash string) (update, error) {
	var info struct {
		Error                  string
		ID, Result, ResMessage string
		BlockNumber            *int64
		Receipt                struct{ Result string }
	}
	_, err := s.nativeHTTP(ctx, p, "/walletsolidity/gettransactioninfobyid", nil, "application/json", bodyJSON(map[string]string{"value": hash}), &info)
	if err != nil {
		return update{}, err
	}
	if info.Error != "" {
		return update{}, errNativeEvidence
	}
	if info.ID == "" && info.BlockNumber == nil {
		return nativeTracking(), nil
	}
	if info.ID != hash || info.BlockNumber == nil {
		return update{}, errNativeEvidence
	}
	var block tronBlock
	_, err = s.nativeHTTP(ctx, p, "/walletsolidity/getblockbynum", nil, "application/json", bodyJSON(map[string]int64{"num": *info.BlockNumber}), &block)
	if err != nil {
		return update{}, err
	}
	index := slices.IndexFunc(block.Transactions, func(tx tronTxRef) bool { return tx.ID == hash })
	if block.ID == "" || block.Header.Data.Number == nil || *block.Header.Data.Number != *info.BlockNumber || index < 0 {
		return update{}, errNativeEvidence
	}
	proof := map[string]any{"blockNumber": *info.BlockNumber, "blockHash": block.ID}
	result := info.Receipt.Result
	if result == "DEFAULT" {
		result = ""
	}
	for _, contract := range block.Transactions[index].Results {
		if contract.Status == "" || contract.Status == "DEFAULT" {
			continue
		}
		if result != "" && result != contract.Status {
			return update{}, errNativeEvidence
		}
		result = contract.Status
	}
	if result == "" && (info.Result == "SUCESS" || info.Result == "SUCCESS") {
		result = "SUCCESS"
	}
	if result == "" && info.Result != "FAILED" {
		return included(hash, "PENDING", proof), nil
	}
	if result == "SUCCESS" && info.Result == "FAILED" {
		return update{}, errNativeEvidence
	}
	ok := result == "SUCCESS"
	if !ok && info.ResMessage != "" {
		proof["executionDetail"] = limitedDetail(tronText(info.ResMessage))
	}
	u := nativeIncluded(hash, ok, proof)
	if !ok {
		switch result {
		case "OUT_OF_ENERGY":
			u.reason = "TRON_OUT_OF_ENERGY"
		case "OUT_OF_TIME":
			u.reason = "TRON_OUT_OF_TIME"
		case "REVERT":
			u.reason = "TRON_REVERTED"
		default:
			if result != "" && result != "DEFAULT" {
				proof["receiptResult"] = limitedDetail(result)
				u.evidence = bodyJSON(proof)
			}
		}
	}
	return u, nil
}
