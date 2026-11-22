package broadcast

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Family string

const (
	EVM    Family = "evm"
	Solana Family = "solana"
)

type Network struct {
	Family Family `json:"family"`
	// EVM networks are keyed by decimal chain ID; Solana networks by cluster name.
	GenesisHash         string   `json:"genesisHash,omitempty"`
	FeePayers           []string `json:"feePayers,omitempty"`
	EntryPoints         []string `json:"entryPoints,omitempty"`
	DefaultProbeFile    string   `json:"defaultProbeFile,omitempty"`
	PublicNodeProbeFile string   `json:"publicNodeProbeFile,omitempty"`
	MaxHeightLag        int64    `json:"maxHeightLag,omitempty"`
	StallSeconds        int      `json:"stallSeconds,omitempty"`
}
type Client struct {
	Name          string `json:"name"`
	Token         string `json:"token"`
	Admin         bool   `json:"admin"`
	Webhook       string `json:"webhook,omitempty"`
	WebhookSecret string `json:"webhookSecret,omitempty"`
}
type GlobalWebhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type Config struct {
	BroadcastNotifications *GlobalWebhook     `json:"broadcastNotifications,omitempty"`
	Listen                 string             `json:"listen"`
	Networks               map[string]Network `json:"networks"`
	Clients                []Client           `json:"clients"`
	Workers                int                `json:"workers"`
	TrackingHours          int                `json:"trackingHours"`
}

type Authorization struct {
	Index         int    `json:"index"`
	Authority     string `json:"authority,omitempty"`
	Delegate      string `json:"delegate"`
	Nonce         string `json:"nonce"`
	Chain         string `json:"chainId"`
	Prediction    string `json:"prediction,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ObservedNonce string `json:"observedNonce,omitempty"`
	ObservedBlock string `json:"observedBlock,omitempty"`
	Result        string `json:"result"`
}
type Version struct {
	Hash           string          `json:"hash"`
	State          string          `json:"state"`
	Raw            string          `json:"-"`
	Blockhash      string          `json:"blockhash,omitempty"`
	Intent         string          `json:"-"`
	ContextSlot    uint64          `json:"contextSlot,omitempty"`
	OriginSlot     uint64          `json:"verifiedOriginSlot,omitempty"`
	LastValid      uint64          `json:"lastValidBlockHeight,omitempty"`
	Authorizations []Authorization `json:"authorizations,omitempty"`
	Broadcasts     []Delivery      `json:"broadcasts,omitempty"`
}
type Task struct {
	ID        string          `json:"taskId"`
	Family    Family          `json:"family"`
	Network   string          `json:"network"`
	Owner     string          `json:"-"`
	Sender    string          `json:"sender"`
	Nonce     string          `json:"nonce,omitempty"`
	Payment   string          `json:"paymentId,omitempty"`
	State     string          `json:"taskState"`
	Active    string          `json:"activeHash"`
	Winner    string          `json:"winningHash,omitempty"`
	Consuming string          `json:"consumingHash,omitempty"`
	Outcome   string          `json:"taskOutcome,omitempty"`
	Execution string          `json:"executionStatus,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
	Versions  []Version       `json:"attempts,omitempty"`
	Revision  int64           `json:"version"`
	Force     bool            `json:"-"`
	Created   time.Time       `json:"createdAt"`
	Next      time.Time       `json:"nextCheckAt,omitzero"`
}
type Delivery struct {
	ID       string    `json:"id"`
	Round    string    `json:"round"`
	Provider string    `json:"providerId"`
	Endpoint string    `json:"endpointId"`
	State    string    `json:"state"`
	Detail   string    `json:"detail,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Started  time.Time `json:"startedAt"`
	Latency  int64     `json:"latencyMs"`
}
type Fault struct {
	Status int
	Code   string
	Detail string
}

func (e *Fault) Error() string { return e.Code + ": " + e.Detail }

func invalid(detail string) error { return &Fault{400, "INVALID_REQUEST", detail} }

func conflict(code string) error { return &Fault{409, code, ""} }

var notFound = &Fault{404, "NOT_FOUND", ""}

func id() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func bodyJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("invalid internal JSON: %v", err))
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, err error) {
	var e *Fault
	if !errors.As(err, &e) {
		e = &Fault{500, "INTERNAL_ERROR", ""}
	}
	writeJSON(w, e.Status, map[string]string{"error": e.Code, "detail": e.Detail})
}
