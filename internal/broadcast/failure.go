package broadcast

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func limitedDetail(value string) string {
	if len(value) <= 512 {
		return value
	}
	return strings.ToValidUTF8(value[:512], "")
}

// A node refusal describes only this delivery. Canonical nonce consumption and
// VM execution are established independently by the chain observer.
func rejectionReason(f Family, detail string) string {
	msg := strings.ToLower(detail)
	switch f {
	case EVM:
		switch {
		case strings.Contains(msg, "nonce too low"):
			return "NONCE_TOO_LOW"
		case strings.Contains(msg, "nonce too high"):
			return "NONCE_TOO_HIGH"
		case strings.Contains(msg, "intrinsic gas too low"):
			return "INTRINSIC_GAS_TOO_LOW"
		case strings.Contains(msg, "insufficient funds"):
			return "INSUFFICIENT_FUNDS"
		case strings.Contains(msg, "underpriced"), strings.Contains(msg, "less than block base fee"), strings.Contains(msg, "gas price too low"), strings.Contains(msg, "gas price below configured minimum"):
			return "FEE_TOO_LOW"
		case strings.Contains(msg, "invalid sender"):
			return "INVALID_SENDER"
		}
	case Tron:
		// tronAdapter has already interpreted the node code and decoded message.
		for _, code := range []string{"TRON_SIGNATURE_WEIGHT_INSUFFICIENT", "TRON_PERMISSION_DENIED", "TRON_SIGNATURE_INVALID", "TRON_TRANSACTION_EXPIRED", "TRON_TAPOS_ERROR", "TRON_BANDWIDTH_ERROR", "TRON_CONTRACT_VALIDATE_ERROR"} {
			if strings.HasSuffix(detail, code) {
				return code
			}
		}
	}
	return "RPC_REJECTED"
}

func tronText(message string) string {
	if b, err := hex.DecodeString(message); err == nil && utf8.Valid(b) {
		return string(b)
	}
	return message
}

func tronRejection(code, message string) string {
	msg := strings.ToLower(tronText(message))
	switch {
	case code == "SIGERROR" && (msg == "sig error" || msg == "validate signature error: sig error"), code == "NOT_ENOUGH_PERMISSION", strings.Contains(msg, "not enough weight"), strings.Contains(msg, "weight") && strings.Contains(msg, "threshold"):
		return "TRON_SIGNATURE_WEIGHT_INSUFFICIENT"
	case code == "PERMISSION_ERROR", strings.Contains(msg, "not contained of permission"), strings.Contains(msg, "permission denied"), strings.Contains(msg, "permission isn't exit"), strings.Contains(msg, "permission type is error"), strings.Contains(msg, "permission is not"), strings.Contains(msg, "permission does not exist"):
		return "TRON_PERMISSION_DENIED"
	case code == "SIGERROR":
		return "TRON_SIGNATURE_INVALID"
	case code == "TRANSACTION_EXPIRATION_ERROR":
		return "TRON_TRANSACTION_EXPIRED"
	case code == "TAPOS_ERROR":
		return "TRON_TAPOS_ERROR"
	case code == "BANDWITH_ERROR", code == "BANDWIDTH_ERROR":
		return "TRON_BANDWIDTH_ERROR"
	case code == "CONTRACT_VALIDATE_ERROR":
		return "TRON_CONTRACT_VALIDATE_ERROR"
	}
	return "RPC_REJECTED"
}

func solanaFailure(raw json.RawMessage) string {
	var kind string
	if json.Unmarshal(raw, &kind) != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil && len(fields) == 1 {
			for k := range fields {
				kind = k
			}
		}
	}
	switch kind {
	case "InstructionError":
		return "SOLANA_INSTRUCTION_ERROR"
	case "InsufficientFundsForFee":
		return "SOLANA_INSUFFICIENT_FUNDS_FOR_FEE"
	case "InsufficientFundsForRent":
		return "SOLANA_INSUFFICIENT_FUNDS_FOR_RENT"
	case "AccountNotFound":
		return "SOLANA_ACCOUNT_NOT_FOUND"
	}
	return "SOLANA_TRANSACTION_ERROR"
}
