package broadcast

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	sol "github.com/gagliardetto/solana-go"
)

func decodeEVM(raw string) (Task, Version, error) {
	var t Task
	var v Version
	data, err := hexutil.Decode(raw)
	if err != nil {
		return t, v, invalid("rawTransaction must be hex")
	}
	var tx types.Transaction
	if err = tx.UnmarshalBinary(data); err != nil {
		return t, v, invalid("invalid EVM transaction encoding")
	}
	if !tx.Protected() || tx.ChainId().Sign() <= 0 {
		return t, v, invalid("a chain-bound signature is required")
	}
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.DynamicFeeTxType, types.SetCodeTxType:
	default:
		return t, v, invalid("unsupported EVM envelope")
	}
	if tx.Gas() < minimumGas(&tx) || tx.Nonce() == math.MaxUint64 || tx.GasFeeCap().Cmp(tx.GasTipCap()) < 0 {
		return t, v, invalid("invalid nonce, gas limit or fee caps")
	}
	if tx.Type() == types.SetCodeTxType && len(tx.SetCodeAuthorizations()) == 0 {
		return t, v, invalid("empty authorization list")
	}
	sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
	if err != nil {
		return t, v, invalid("invalid outer signature")
	}
	t = Task{Family: EVM, Network: tx.ChainId().String(), Sender: strings.ToLower(sender.Hex()), Nonce: strconv.FormatUint(tx.Nonce(), 10), State: "QUEUED"}
	v = Version{Hash: tx.Hash().Hex(), State: "QUEUED", Raw: hexutil.Encode(data)}
	for i, auth := range tx.SetCodeAuthorizations() {
		a := Authorization{Index: i, Delegate: auth.Address.Hex(), Nonce: strconv.FormatUint(auth.Nonce, 10), Chain: auth.ChainID.ToBig().String(), Result: "UNKNOWN"}
		addr, e := auth.Authority()
		if e == nil {
			a.Authority = strings.ToLower(addr.Hex())
		}
		switch {
		case e != nil:
			a.Reason = "INVALID_AUTH_SIGNATURE"
		case !auth.ChainID.IsZero() && auth.ChainID.ToBig().Cmp(tx.ChainId()) != 0:
			a.Reason = "AUTH_CHAIN_MISMATCH"
		case auth.Nonce == math.MaxUint64:
			a.Reason = "AUTH_NONCE_OVERFLOW"
		}
		if a.Reason != "" {
			a.Prediction = "SKIPPED"
			a.Result = "SKIPPED"
		}
		v.Authorizations = append(v.Authorizations, a)
	}
	return t, v, nil
}

// minimumGas is the intrinsic lower bound shared by the supported
// post-Pectra transaction envelopes. Chain-specific rules stay with the node.
func minimumGas(tx *types.Transaction) uint64 {
	gas := uint64(21000)
	if tx.To() == nil {
		gas += 32000 + 2*uint64((len(tx.Data())+31)/32)
	}
	for _, b := range tx.Data() {
		if b == 0 {
			gas += 4
		} else {
			gas += 16
		}
	}
	for _, tuple := range tx.AccessList() {
		gas += 2400 + 1900*uint64(len(tuple.StorageKeys))
	}
	return gas + 25000*uint64(len(tx.SetCodeAuthorizations()))
}

type SolanaRequest struct {
	Blockhash string `json:"blockhash,omitempty"`
	Cluster   string `json:"cluster"`
	Payment   string `json:"paymentId,omitempty"`
	Signed    string `json:"signedTransaction"`
	Context   struct {
		Slot uint64 `json:"slot"`
	} `json:"context"`
	LastValid uint64 `json:"lastValidBlockHeight"`
}

func decodeSolana(r SolanaRequest, n Network) (Task, Version, error) {
	var t Task
	var v Version
	raw, err := base64.StdEncoding.Strict().DecodeString(r.Signed)
	if err != nil || len(raw) > 1232 || len(raw) < 130 {
		return t, v, invalid("invalid legacy/v0 transaction bytes")
	}
	tx, err := sol.TransactionFromBytes(raw)
	if err != nil || tx.Message.GetVersion() == sol.MessageVersionV1 {
		return t, v, invalid("expected legacy or v0 transaction")
	}
	m := &tx.Message
	count := int(m.Header.NumRequiredSignatures)
	if count < 2 || count != len(tx.Signatures) || count > len(m.AccountKeys) || int(m.Header.NumReadonlySignedAccounts) >= count || int(m.Header.NumReadonlyUnsignedAccounts) > len(m.AccountKeys)-count {
		return t, v, invalid("fee payer and user signatures are required")
	}
	seen := make(map[sol.PublicKey]bool, len(m.AccountKeys))
	for _, k := range m.AccountKeys {
		if seen[k] {
			return t, v, invalid("duplicate static account")
		}
		seen[k] = true
	}
	if !slices.Contains(n.FeePayers, m.AccountKeys[0].String()) {
		return t, v, invalid("unrecognized fee payer")
	}
	total := len(m.AccountKeys)
	for _, l := range m.AddressTableLookups {
		total += len(l.WritableIndexes) + len(l.ReadonlyIndexes)
	}
	if total > 256 || len(m.Instructions) == 0 {
		return t, v, invalid("invalid account or instruction count")
	}
	for i, ix := range m.Instructions {
		if ix.ProgramIDIndex == 0 || int(ix.ProgramIDIndex) >= len(m.AccountKeys) {
			return t, v, invalid("invalid program index")
		}
		for _, acct := range ix.Accounts {
			if int(acct) >= total {
				return t, v, invalid("invalid account index")
			}
		}
		if i == 0 && m.AccountKeys[ix.ProgramIDIndex] == sol.SystemProgramID && len(ix.Data) >= 4 && binary.LittleEndian.Uint32(ix.Data) == 4 {
			return t, v, invalid("durable nonce is unsupported")
		}
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, raw) || tx.VerifySignatures() != nil {
		return t, v, invalid("invalid Solana signatures or noncanonical message")
	}
	if r.LastValid == 0 || r.Context.Slot == 0 || r.LastValid > math.MaxInt64 || r.Context.Slot > math.MaxInt64 {
		return t, v, invalid("valid blockhash context and expiry are required")
	}
	if r.Blockhash != "" && r.Blockhash != m.RecentBlockhash.String() {
		return t, v, invalid("blockhash does not match signed message")
	}
	v = Version{Hash: tx.Signatures[0].String(), Raw: r.Signed, Blockhash: m.RecentBlockhash.String(), State: "READY", LastValid: r.LastValid, ContextSlot: r.Context.Slot}
	// A resign may change only the recent blockhash and signatures.
	m.RecentBlockhash = sol.Hash{}
	msg, err := m.MarshalBinary()
	if err != nil {
		return t, v, invalid("invalid Solana message")
	}
	digest := sha256.Sum256(msg)
	v.Intent = hex.EncodeToString(digest[:])
	t = Task{Family: Solana, Network: r.Cluster, Sender: m.AccountKeys[0].String(), Payment: r.Payment, State: "READY"}
	return t, v, nil
}
