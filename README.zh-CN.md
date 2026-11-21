# Broadcaster

GM Wallet 的 18 条主网已签名交易广播服务。钱包或代付服务提交签名原文，Broadcaster 向最多三家独立 RPC 供应商并发广播，持续核查入块和执行结果，并通知提交方。

[English](README.md) · 简体中文

Go 1.26.2+ · SQLite · [MIT](LICENSE)

[快速开始](#快速开始) · [支持的网络](#支持的网络) · [提交交易](#提交交易) · [接口与生命周期](docs/api.zh-CN.md)

## 一笔交易如何完成

1. 钱包或签名服务构造并签名。Broadcaster 把原文写入 SQLite，返回可查询的交易标识。
2. 每轮从同链选取最多三家独立供应商并发发送。只有两家或一家健康时按实际数量发送。某家超时后，任务仍会查链核实结果。
3. 找到该链的入块证据后，服务记录入块位置和执行结果。提交方可以查询任务，也可以用带签名的 Webhook 接收通知。任务关闭时保存的是当时观测到的链上快照。

EVM 签名前可以查询推荐 nonce。服务先随机选健康 RPC 读取链上值，再跳过该发送账户活动队列里已占用的槽位。Solana 代付由用户和代付方签署同一份消息。若 recent blockhash 过期，服务核查历史后再通知双方重签。[交易调度与状态机](docs/api.zh-CN.md)

## 快速开始

单链试运行使用以太坊示例。部署全部 18 条主网时，改用 `cp config.mainnets.json config.json`，并将 `SOLANA_FEE_PAYER` 环境变量设为代付服务的真实公钥。首次生成的令牌及存储密钥应保存供重启时使用。

```sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
./broadcaster -config config.json -db broadcaster.db
```

为要启用的链注册供应商。以下示例使用 Bitcoin Core JSON-RPC 2.0 网关，节点须已同步交易索引。将 `BTC_RPC_URL` 设为网关地址，`BTC_PROBE_HEX` 设为准备实际发送的已签名测试交易。准入会核对主网、读取权限和真实写权限。

```sh
BASE=http://127.0.0.1:8080
jq -n --arg url "$BTC_RPC_URL" --arg raw "$BTC_PROBE_HEX" \
  '{network:"bitcoin",providerId:"provider-a",url:$url,nativeProbe:{rawTransaction:$raw}}' |
  curl -fsS -X POST "$BASE/rpc-providers" -H "Authorization: Bearer $BROADCASTER_ADMIN_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @-
```

使用另外两家供应商各自的 ID 和端点重复注册，即可三路并发。部分网络提供 PublicNode 预设，也须通过准入才能使用。[18 链配置](config.mainnets.json) · [节点准入、熔断与恢复](docs/api.zh-CN.md#rpc-节点管理)

## 支持的网络

网络清单与 ChainScanner 的节点目录对齐。ChainScanner 的交易扫描注册表包含 17 条，Bitcoin 单独提供节点高度探测。Broadcaster 为 Bitcoin 提供交易广播和入块查询，两套服务各自直连 RPC。[ChainScanner 扫描注册表](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/chains/networks.go) · [Bitcoin 节点目录](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/nodes/catalog.go)

| EVM 主网 | Chain ID | EVM 主网 | Chain ID |
|---|---:|---|---:|
| Ethereum | 1 | Base | 8453 |
| Arbitrum One | 42161 | HyperEVM | 999 |
| BNB Smart Chain | 56 | Monad | 143 |
| Avalanche C-Chain | 43114 | Arc | 5042 |
| Polygon PoS | 137 | Plasma | 9745 |
| OP Mainnet | 10 | Robinhood Chain | 4663 |

EVM 用十进制 chain ID 标识网络，Broadcaster 从签名原文解析实际 chain ID，并按交易哈希查询。HyperEVM 按官方公布的 Cancun 规则准入普通交易。其余 EVM 网络同时验证普通交易和 Type 4 广播能力，探针通过后才启用该端点。[HyperEVM 协议说明](https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/hyperevm)

| 网络键 | 签名服务提交的内容 | 关闭任务的入块证据 |
|---|---|---|
| `solana` | 已签齐的 legacy 或 v0 交易和 recent blockhash 有效期信息 | 已确认或最终确定的签名及对应 slot 证据 |
| `bitcoin` | 十六进制 wire 交易，保留隔离见证 | txid 所在块仍在活动链，确认数大于零 |
| `tron` | 十六进制已签名 protobuf Transaction | 在固化块交易列表中找到相同 txID，并读取执行回执 |
| `aptos` | Base64 编码的 BCS SignedTransaction | 已提交的用户交易、账本版本及执行结果 |
| `sui` | Base64 编码的 BCS TransactionData 及独立签名数组 | 相同交易摘要、checkpoint 和执行结果 |
| `ton` | Base64 编码的外部入站消息 BOC | 入站消息规范化哈希匹配，并找到接收账户交易及分片块位置 |

## 提交交易

Bitcoin、TRON、Aptos、Sui 和 TON 使用相同的 HTTP 路径结构。以 Bitcoin 为例，`BTC_SIGNED_HEX` 是本次付款的已签名原文。

```sh
TX_HASH=$(jq -n --arg raw "$BTC_SIGNED_HEX" '{rawTransaction:$raw}' |
  curl -fsS "$BASE/networks/bitcoin/transactions" -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- | jq -er .txHash)
curl -fsS "$BASE/networks/bitcoin/transactions/$TX_HASH" -H "Authorization: Bearer $BROADCASTER_API_TOKEN"
```

EVM 继续使用 `POST /transactions`，Solana 代付继续使用 `POST /solana/transactions`。HTTP 202 表示任务已持久化。查询时用 `taskOutcome` 判断是否入块，用 `executionStatus` 获取执行结果。TON 的结果对应收到外部消息的那笔账户交易，后续合约消息需由业务方分别核实。[原生链接口](docs/api.zh-CN.md#原生链广播接口) · [EVM 接口](docs/api.zh-CN.md#evm-调用方接口) · [Solana 代付](docs/api.zh-CN.md#solana-代付交易)
