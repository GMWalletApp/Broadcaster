# Broadcaster

Submit signed transactions across 18 mainnets and follow each one from RPC submission to on-chain inclusion. GM Wallet uses Broadcaster to send the same signed payload to up to three independent providers and report the outcome to the service that submitted it.

English · [简体中文](README.zh-CN.md)

Go 1.26.2+ · SQLite · [MIT](LICENSE)

[Quick start](#quick-start) · [Networks](#networks) · [Submit a transaction](#submit-a-transaction) · [API and lifecycle guide (中文)](docs/api.zh-CN.md)

## How it works

1. Your wallet or relayer constructs and signs the transaction. Broadcaster records the payload in SQLite and returns its identifier.
2. A worker sends it concurrently to three admitted providers when available, or to two or one when fewer are healthy. A timeout or lost response leaves the transaction under observation.
3. The worker checks chain-specific inclusion evidence and records execution success or failure. Applications can poll the task or receive a signed webhook. The stored inclusion snapshot represents what the service observed when it closed the task.

EVM callers can query a nonce before signing. The service reads a randomly selected healthy RPC and skips occupied slots in the sender’s active queue. On Solana, the user and fee payer sign the same message. If the recent blockhash expires, Broadcaster checks transaction history before requesting new signatures. [API and lifecycle guide](docs/api.zh-CN.md)

## Quick start

Build with Go 1.26.2 or later. The short configuration starts an Ethereum service. For all 18 mainnets, copy `config.mainnets.json` instead and set `SOLANA_FEE_PAYER` to your signing service’s public key. Keep the generated tokens and storage key for subsequent restarts.

```sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
./broadcaster -config config.json -db broadcaster.db
```

Register an RPC for each network you use. Admission checks network identity, read access and an actual signed broadcast. Prepare a funded probe that you intend to send. In this Bitcoin example, the HTTPS gateway must expose Bitcoin Core JSON-RPC 2.0 with a synchronized transaction index. `BTC_PROBE_HEX` contains a fully signed wire transaction.

```sh
BASE=http://127.0.0.1:8080
jq -n --arg url "$BTC_RPC_URL" --arg raw "$BTC_PROBE_HEX" \
  '{network:"bitcoin",providerId:"provider-a",url:$url,nativeProbe:{rawTransaction:$raw}}' |
  curl -fsS -X POST "$BASE/rpc-providers" -H "Authorization: Bearer $BROADCASTER_ADMIN_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @-
```

Repeat with a distinct provider ID and endpoint to add another operator. PublicNode presets cover a subset of the networks and also require admission. [18-network configuration](config.mainnets.json) · [Provider admission, defaults and recovery](docs/api.zh-CN.md#rpc-节点管理)

## Networks

The network names follow GM Wallet’s ChainScanner inventory. The scanner has 17 transaction-scanning networks and lists Bitcoin separately for node-height probes. Broadcaster includes Bitcoin transaction submission and tracking. Each service talks directly to its providers. [ChainScanner network registry](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/chains/networks.go) · [Bitcoin node inventory](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/nodes/catalog.go)

| EVM mainnet | Chain ID | EVM mainnet | Chain ID |
|---|---:|---|---:|
| Ethereum | 1 | Base | 8453 |
| Arbitrum One | 42161 | HyperEVM | 999 |
| BNB Smart Chain | 56 | Monad | 143 |
| Avalanche C-Chain | 43114 | Arc | 5042 |
| Polygon PoS | 137 | Plasma | 9745 |
| OP Mainnet | 10 | Robinhood Chain | 4663 |

EVM networks use the decimal chain ID for configuration, the protected signed envelope for submission and the transaction hash for lookup. HyperEVM uses ordinary transaction admission under its published Cancun rules. The other EVM pools require a successful ordinary and Type 4 probe before a provider can send traffic. [HyperEVM protocol](https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/hyperevm)

| Network key | Signed input | Evidence that closes the task |
|---|---|---|
| `solana` | Serialized legacy or v0 transaction with all required signatures, plus recent-blockhash metadata | Confirmed or finalized signature with slot evidence |
| `bitcoin` | Hex wire transaction, including witnesses | Matching txid in a block with positive confirmations on the active chain |
| `tron` | Hex signed protobuf Transaction | Matching txID in a solidified block, plus its receipt |
| `aptos` | Base64 BCS SignedTransaction | Committed user transaction at a ledger version, with execution status |
| `sui` | Base64 BCS TransactionData and a separate base64 signatures array | Matching digest, checkpoint and execution effects |
| `ton` | Base64 external inbound message BOC | Account transaction matching the normalized incoming message hash and shard block reference |

## Submit a transaction

Use a business token. For Bitcoin, TRON, Aptos, Sui and TON, submit to `POST /networks/{network}/transactions` and query `GET /networks/{network}/transactions/{txHash}`. Use a fresh signed payment for this example.

```sh
TX_HASH=$(jq -n --arg raw "$BTC_SIGNED_HEX" '{rawTransaction:$raw}' |
  curl -fsS "$BASE/networks/bitcoin/transactions" -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- | jq -er .txHash)
curl -fsS "$BASE/networks/bitcoin/transactions/$TX_HASH" -H "Authorization: Bearer $BROADCASTER_API_TOKEN"
```

EVM uses `POST /transactions` and Solana uses `POST /solana/transactions`. HTTP 202 acknowledges durable intake. Check `taskOutcome` for inclusion and `executionStatus` for the recorded execution result. On TON this describes the receiving wallet transaction. Downstream messages may have their own outcomes. [Native network API](docs/api.zh-CN.md#原生链广播接口) · [EVM API](docs/api.zh-CN.md#evm-调用方接口) · [Solana sponsorship](docs/api.zh-CN.md#solana-代付交易)
