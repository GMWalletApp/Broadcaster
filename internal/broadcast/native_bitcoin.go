package broadcast

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/wire"
)

const bitcoinMainnetGenesis = "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"

type bitcoinAdapter struct{}

func (bitcoinAdapter) decode(in NativeRequest) (Version, error) {
	raw, err := hex.DecodeString(in.Raw)
	if err != nil {
		return Version{}, invalid("expected Bitcoin wire transaction in hex")
	}
	r := bytes.NewReader(raw)
	var tx wire.MsgTx
	if tx.Deserialize(r) != nil || r.Len() != 0 || len(tx.TxIn) == 0 || len(tx.TxOut) == 0 {
		return Version{}, invalid("invalid Bitcoin transaction")
	}
	signed := false
	for _, input := range tx.TxIn {
		if input.PreviousOutPoint.Index == 0xffffffff && input.PreviousOutPoint.Hash == ([32]byte{}) {
			return Version{}, invalid("coinbase cannot be broadcast")
		}
		signed = signed || len(input.SignatureScript) > 0 || len(input.Witness) > 0
	}
	if !signed {
		return Version{}, invalid("missing Bitcoin unlocking script or witness")
	}
	return nativeVersion(tx.TxHash().String(), hex.EncodeToString(raw)), nil
}

func (bitcoinAdapter) height(ctx context.Context, s *Service, p Endpoint) (int64, error) {
	var info struct {
		Chain  string
		Blocks *int64
	}
	if err := s.call(ctx, p, "getblockchaininfo", []any{}, &info); err != nil {
		return 0, err
	}
	if info.Chain != "main" || info.Blocks == nil || *info.Blocks < 0 {
		return 0, errNativeEvidence
	}
	var genesis string
	if err := s.call(ctx, p, "getblockhash", []any{0}, &genesis); err != nil {
		return 0, err
	}
	if genesis != bitcoinMainnetGenesis {
		return 0, errNativeEvidence
	}
	return *info.Blocks, nil
}

func (bitcoinAdapter) send(ctx context.Context, s *Service, p Endpoint, v Version) error {
	var hash string
	err := s.call(ctx, p, "sendrawtransaction", []any{v.Raw}, &hash)
	var rpc *rpcError
	if errors.As(err, &rpc) && rpc.Code == -27 {
		return &rpcError{Message: "already known"}
	}
	if err == nil && hash != v.Hash {
		return errNativeEvidence
	}
	return err
}

func (bitcoinAdapter) lookup(ctx context.Context, s *Service, p Endpoint, hash string) (update, error) {
	var tx struct {
		TxID          string
		BlockHash     string
		Confirmations int64
	}
	err := s.call(ctx, p, "getrawtransaction", []any{hash, true}, &tx)
	var rpc *rpcError
	if errors.As(err, &rpc) && rpc.Code == -5 {
		return nativeTracking(), nil
	}
	if err != nil {
		return update{}, err
	}
	if tx.TxID != hash {
		return update{}, errNativeEvidence
	}
	if tx.Confirmations < 1 {
		return nativeTracking(), nil
	}
	var header struct {
		Hash          string
		Height        *int64
		Confirmations int64
	}
	if err = s.call(ctx, p, "getblockheader", []any{tx.BlockHash, true}, &header); err != nil {
		return update{}, err
	}
	if header.Hash == "" || header.Hash != tx.BlockHash || header.Height == nil || *header.Height < 0 || header.Confirmations < 1 {
		return update{}, errNativeEvidence
	}
	var active string
	if err = s.call(ctx, p, "getblockhash", []any{*header.Height}, &active); err != nil {
		return update{}, err
	}
	if active != header.Hash {
		return nativeTracking(), nil
	}
	return nativeIncluded(hash, true, map[string]any{"blockHash": header.Hash, "blockNumber": *header.Height}), nil
}
