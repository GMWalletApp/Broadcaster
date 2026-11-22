<p align="center"><img src="docs/assets/broadcaster-logo.png" width="80" height="80" alt="Broadcaster"></p>
<h1 align="center">Broadcaster</h1>
<p align="center"><strong>Submit a signed transaction and receive its on-chain result in your service.</strong></p>
<p align="center"><strong>English</strong> · <a href="README.zh-CN.md">简体中文</a></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">AGPL-3.0</a> · 18 mainnets</p>
<p align="center"><a href="#quick-start">Quick start</a> · <a href="#networks">Networks</a> · <a href="docs/api.md">API reference</a></p>

---

## What happens to a payment

Your wallet or payment service signs a transaction and submits it once. Broadcaster saves the signed bytes in SQLite, sends them through up to three independent RPC providers, and checks what landed. A node can time out after accepting a transaction. The worker keeps checking the original hash and resumes after a restart.

For an EVM transfer, Broadcaster coordinates submitted nonces and watches every signed replacement. For a sponsored Solana payment, the user and fee payer sign the same message. When finalized history proves the blockhash expired without inclusion, Broadcaster reports the failed attempt to the submitting service. The application decides what happens next.

The optional BalanceKeeper integration forwards accepted and included transaction identifiers from every submitting client to one global receiver. BalanceKeeper selects the wallets to refresh. [Notification contract](docs/api.md#balancekeeper)

## Quick start

### 1. Configure the service

The examples use Go 1.26.2+, OpenSSL, curl and jq. From the repository root, build the server and copy the Ethereum example. Generate the credentials once and retain them for restarts.

```sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
```

To receive callbacks, set the wallet client’s `webhook` to your HTTPS receiver and `webhookSecret` to a separate secret of at least 24 bytes. Save them in your private deployment configuration. [Configuration](docs/api.md#setup)

### 2. Enable a provider and start the worker

Put authorized, signed admission probes in `probes/1.json`. Startup checks the chain and broadcasts these probes on mainnet. A provider that passes becomes eligible for wallet traffic. You can also register one later with the admin token. [Probe format and provider setup](docs/api.md#providers)

```sh
./broadcaster -config config.json -db broadcaster.db
```

### 3. Submit a signed transfer

In a second shell with the same business token, set `SIGNED_TX` to the hex-encoded signed transaction. Save the returned hash against your payment.

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/EVM/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

## Receive the result

HTTP 202 means the task was saved. With a callback configured, the submitting service receives a signed `transaction.closed` event when the task ends. Use the two result fields to decide how to update the order.

| Result | Meaning |
|---|---|
| `CONFIRMING` + `PENDING` | Inclusion observed. Keep the payment open while the worker checks execution. |
| `INCLUDED` + `SUCCESS` | Execution succeeded. Apply any additional settlement confirmations your service requires. |
| `INCLUDED` + `FAILED` | Execution failed. Read `reason` and `evidence` for the available cause. |
| `EXPIRED` + `BLOCKHASH_EXPIRED` | The Solana transaction expired without inclusion. The submitting service receives a `transaction.closed` failure notification. |
| `EXPIRED` + `TRACKING_DEADLINE` | Local tracking timed out. Reconcile the payment before sending again. |
| `REPLACED_EXTERNALLY` | Another transaction consumed the nonce. Reconcile the consuming hash. |

Verify the callback signature, persist the event and deduplicate by `eventId`. To reconcile a missed notification, query the original hash. [Webhook contract](docs/api.md#webhooks-and-recovery)

```sh
curl -fsS "$BASE/EVM/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus, reason}'
```

## Networks

| Mainnets | Submission endpoint |
|---|---|
| Ethereum · Arbitrum One · BNB Smart Chain · Avalanche C-Chain · Polygon PoS · OP Mainnet · Base · HyperEVM · Monad · Arc · Plasma · Robinhood Chain | `POST /EVM/transactions` |
| Solana | `POST /solana/transactions` |
| Bitcoin · TRON · Aptos · Sui · TON | `POST /networks/{network}/transactions` |

Each mainnet has one preset. Fifteen use PublicNode. Monad uses Monad Foundation, Plasma uses its public RPC, and TON uses TON Center. Register additional independent providers privately. Routes marked MEV-Protected are selected first. Sending the same bytes to a public mempool also exposes that copy. [Mainnet configuration](config.mainnets.json) · [Provider catalog and limits](docs/api.md#providers)

## Integration reference

- [EVM transactions, nonce queries, force and replacement](docs/api.md#evm-api)
- [Solana sponsorship and the five native transaction formats](docs/api.md#non-evm-api)
- [Lifecycle states, execution failures and recovery](docs/api.md#lifecycle)
- [Provider admission, circuit breakers and deployment](docs/api.md#providers)
