package broadcast

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"time"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
	"github.com/mr-tron/base58"
	"golang.org/x/crypto/blake2b"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type suiAdapter struct{}

func (suiAdapter) decode(in NativeRequest) (Version, error) {
	raw, err := base64.StdEncoding.DecodeString(in.Raw)
	if err != nil || len(raw) < 32 || raw[0] != 0 || len(in.Signatures) < 1 || len(in.Signatures) > 16 {
		return Version{}, invalid("expected Sui TransactionData V1 BCS and signatures in base64")
	}
	for _, sig := range in.Signatures {
		data, err := base64.StdEncoding.DecodeString(sig)
		if err != nil || len(data) < 2 {
			return Version{}, invalid("invalid Sui signature encoding")
		}
	}
	digest := blake2b.Sum256(append([]byte("TransactionData::"), raw...))
	return nativeVersion(base58.Encode(digest[:]), string(bodyJSON(in))), nil
}

// Use a bounded call even when the caller has a long-lived worker context.
// TLS and per-endpoint bearer credentials follow the configured HTTP(S) URL.
func suiClient(ctx context.Context, p Endpoint) (context.Context, *grpc.ClientConn, func(), error) {
	u, err := url.Parse(p.secret.URL)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return ctx, nil, nil, invalid("Sui gRPC requires a host URL without a path or query")
	}
	var creds credentials.TransportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	port := u.Port()
	if u.Scheme == "http" {
		creds = insecure.NewCredentials()
		if port == "" {
			port = "80"
		}
	}
	if port == "" {
		port = "443"
	}
	conn, err := grpc.NewClient("dns:///"+net.JoinHostPort(u.Hostname(), port), grpc.WithTransportCredentials(creds))
	if err != nil {
		return ctx, nil, nil, errNativeEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if p.secret.Bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+p.secret.Bearer)
	}
	return ctx, conn, func() { cancel(); conn.Close() }, nil
}

func (suiAdapter) height(ctx context.Context, _ *Service, p Endpoint) (int64, error) {
	ctx, c, close, err := suiClient(ctx, p)
	if err != nil {
		return 0, err
	}
	defer close()
	r, err := pb.NewLedgerServiceClient(c).GetServiceInfo(ctx, &pb.GetServiceInfoRequest{})
	if err != nil {
		return 0, errNativeEvidence
	}
	if r.GetChainId() != "4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S" || r.CheckpointHeight == nil {
		return 0, errNativeEvidence
	}
	return int64(r.GetCheckpointHeight()), nil
}

func (suiAdapter) send(ctx context.Context, _ *Service, p Endpoint, v Version) error {
	var in NativeRequest
	if json.Unmarshal([]byte(v.Raw), &in) != nil {
		return errNativeEvidence
	}
	data, err := base64.StdEncoding.DecodeString(in.Raw)
	if err != nil {
		return errNativeEvidence
	}
	req := &pb.ExecuteTransactionRequest{Transaction: &pb.Transaction{Bcs: &pb.Bcs{Value: data}}, ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"digest"}}}
	for _, sig := range in.Signatures {
		b, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			return errNativeEvidence
		}
		req.Signatures = append(req.Signatures, &pb.UserSignature{Bcs: &pb.Bcs{Value: b}})
	}
	ctx, c, close, err := suiClient(ctx, p)
	if err != nil {
		return err
	}
	defer close()
	r, err := pb.NewTransactionExecutionServiceClient(c).ExecuteTransaction(ctx, req)
	if status.Code(err) == codes.InvalidArgument {
		return &rpcError{Message: "Sui transaction rejected"}
	}
	if err != nil || r.GetTransaction().GetDigest() != v.Hash {
		return errNativeEvidence
	}
	return nil
}

func (suiAdapter) lookup(ctx context.Context, _ *Service, p Endpoint, hash string) (update, error) {
	ctx, c, close, err := suiClient(ctx, p)
	if err != nil {
		return update{}, err
	}
	defer close()
	r, err := pb.NewLedgerServiceClient(c).GetTransaction(ctx, &pb.GetTransactionRequest{Digest: &hash, ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"digest", "checkpoint", "effects.status"}}})
	if status.Code(err) == codes.NotFound {
		return nativeTracking(), nil
	}
	if err != nil {
		return update{}, errNativeEvidence
	}
	tx := r.GetTransaction()
	if tx.GetDigest() != hash {
		return update{}, errNativeEvidence
	}
	if tx.Checkpoint == nil {
		return update{state: "TRACKING"}, nil
	}
	st := tx.GetEffects().GetStatus()
	if st == nil || st.Success == nil {
		return update{}, errNativeEvidence
	}
	return nativeIncluded(hash, st.GetSuccess(), map[string]any{"checkpoint": tx.GetCheckpoint()}), nil
}
