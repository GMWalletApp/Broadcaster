<h1 align="center">Broadcaster</h1>
<p align="center">把签好的交易发往 18 条主网，查询它是否入块、执行是否成功。</p>
<p align="center"><a href="README.md">English</a> · <strong>简体中文</strong></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">MIT</a></p>

[提交一笔交易](#提交一笔以太坊交易) · [部署服务](docs/api.zh-CN.md#运行) · [支持的网络](#支持的网络) · [接口文档](docs/api.zh-CN.md)

## 为什么做 Broadcaster

钱包发起一笔转账，RPC 却超时了。节点可能已经收到了交易，只是响应没回来。这时，钱包需要知道原来那笔交易后来怎么样了。

Broadcaster 先把签名原文存到 SQLite，再把同一笔交易同时发给最多三家独立的 RPC 供应商。即使某家超时，服务也会继续查链；进程重启后会接着处理原任务。私钥留在签名方，钱包可以按交易哈希查结果，也可以接收带签名的 Webhook。

## 提交一笔以太坊交易

先按部署指南启动服务并为以太坊启用一个节点。下面的例子使用 `curl` 和 `jq`。将 `SIGNED_TX` 设为钱包已经签好、准备发往以太坊主网的十六进制交易原文，`BROADCASTER_API_TOKEN` 使用部署时设置的业务令牌。[部署服务](docs/api.zh-CN.md#运行) · [启用 PublicNode](docs/api.zh-CN.md#publicnode-默认节点)

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

首次提交返回 HTTP 202，表示服务已记下这笔交易。可以用返回的哈希查进度。

```sh
curl -fsS "$BASE/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus}'
```

| 查到的结果 | 钱包可以告诉用户什么 |
|---|---|
| `taskState: QUEUED` | 交易已排队，正在等待广播。 |
| `taskOutcome: INCLUDED`，`executionStatus: SUCCESS` | 交易已入块，执行成功。 |
| `taskOutcome: INCLUDED`，`executionStatus: FAILED` | 交易已入块，但链上执行失败。 |
| `taskOutcome: INCLUDED`，`executionStatus: UNKNOWN` | 已看到交易入块，稍后再查执行结果。 |

如果付款需要等待多个确认，业务方按自己的结算规则继续等待。[完整状态机](docs/api.zh-CN.md#evm-交易生命周期)

## 支持的网络

| 主网 | 提交入口 |
|---|---|
| Ethereum、Arbitrum One、BNB Smart Chain、Avalanche C-Chain、Polygon PoS、OP Mainnet、Base、HyperEVM、Monad、Arc、Plasma、Robinhood Chain | `POST /transactions` |
| Solana | `POST /solana/transactions` |
| Bitcoin、TRON、Aptos、Sui、TON | `POST /networks/{network}/transactions` |

EVM 签名前可以查询推荐 nonce。比如链上返回 20，队列已占用 20、21、23，服务会推荐 22。Solana 代付时，用户和代付方先签署同一份消息；如果 blockhash 过期，服务会核查原交易是否入块，再决定是否要求重签。[EVM nonce 与替换](docs/api.zh-CN.md#evm-nonce-查询与任务排序) · [Solana 代付](docs/api.zh-CN.md#solana-代付交易) · [其他五条链的签名格式](docs/api.zh-CN.md#原生链广播接口)

## 生产环境怎么配节点

公开配置只内置 PublicNode。每条链独立选点，有一家健康供应商就发一家，有三家就三家并发。

| 想在生产环境使用三家独立供应商 | 需要配置什么 |
|---|---|
| PublicNode 有预设的 15 条链 | 启用 PublicNode，再私下登记两家付费供应商。 |
| Monad、Plasma、TON | 每条链自行登记三家供应商。 |

调度器优先使用标有 MEV-Protected 的端点。如果同一笔交易也发给普通 RPC，该副本仍可能进入公开交易池。[PublicNode 端点和保护标记](docs/api.zh-CN.md#publicnode-默认节点) · [18 条主网配置](config.mainnets.json)
