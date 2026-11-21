package broadcast

import (
	"context"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"strconv"
)

type aptosAdapter struct{}

func (aptosAdapter) decode(in NativeRequest) (Version, error) {
	raw, err := base64.StdEncoding.DecodeString(in.Raw)
	if err != nil || len(raw) < 64 {
		return Version{}, invalid("expected mainnet Aptos BCS SignedTransaction in base64")
	}
	// Hash the exact signed bytes. Aptos fullnodes validate the BCS payload,
	// authenticator and chain ID when admitting the transaction.
	prefix := sha3.Sum256([]byte("APTOS::Transaction"))
	h := sha3.New256()
	h.Write(prefix[:])
	h.Write([]byte{0})
	h.Write(raw)
	return nativeVersion("0x"+hex.EncodeToString(h.Sum(nil)), base64.StdEncoding.EncodeToString(raw)), nil
}

func (aptosAdapter) height(ctx context.Context, s *Service, p Endpoint) (int64, error) {
	var info struct {
		Chain   int    `json:"chain_id"`
		Version string `json:"ledger_version"`
	}
	if _, err := s.nativeHTTP(ctx, p, "", nil, "", nil, &info); err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(info.Version, 10, 64)
	if err != nil || n < 0 || info.Chain != 1 {
		return 0, errNativeEvidence
	}
	return n, nil
}

func (aptosAdapter) send(ctx context.Context, s *Service, p Endpoint, v Version) error {
	raw, err := base64.StdEncoding.DecodeString(v.Raw)
	if err != nil {
		return errNativeEvidence
	}
	var reply struct{ Hash string }
	_, err = s.nativeHTTP(ctx, p, "/transactions", nil, "application/x.aptos.signed_transaction+bcs", raw, &reply)
	if err == nil && reply.Hash != v.Hash {
		return errNativeEvidence
	}
	return err
}

func (aptosAdapter) lookup(ctx context.Context, s *Service, p Endpoint, hash string) (update, error) {
	var tx struct {
		Hash, Type, Version string
		Success             *bool
	}
	status, err := s.nativeHTTP(ctx, p, "/transactions/by_hash/"+hash, nil, "", nil, &tx)
	if status == 404 && err == nil {
		return nativeTracking(), nil
	}
	if err != nil {
		return update{}, err
	}
	if tx.Hash != hash {
		return update{}, errNativeEvidence
	}
	if tx.Type == "pending_transaction" {
		return nativeTracking(), nil
	}
	if _, err := strconv.ParseUint(tx.Version, 10, 64); err != nil || tx.Type != "user_transaction" || tx.Success == nil {
		return update{}, errNativeEvidence
	}
	return nativeIncluded(hash, *tx.Success, map[string]string{"ledgerVersion": tx.Version}), nil
}
