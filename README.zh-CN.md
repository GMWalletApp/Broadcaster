<p align="center"><img src="https://github.com/GMWalletApp.png?size=96" width="80" height="80" alt="GM Wallet"></p>
<h1 align="center">Broadcaster</h1>
<p align="center"><strong>提交已签名交易，由 Broadcaster 广播、跟踪并将链上结果回传给提交服务。</strong></p>
<p align="center"><a href="README.md">English</a> · <strong>简体中文</strong></p>
<p align="center">Go 1.26.2+ · SQLite · <a href="LICENSE">GM Wallet Community License 2.1</a> · 18 条主网</p>
<p align="center"><a href="#快速开始">快速开始</a> · <a href="#支持的网络">支持的网络</a> · <a href="docs/api.zh-CN.md">接口文档</a></p>

公司及其关联实体过去 12 个月的全球合计收入严格超过 100 万美元，即使亏损也须购买许可。按社区条款，修改版在发布、交付或上线运行时须免费向公众提供该版本的完整对应源码，显著保留指定署名及原项目链接。另行签署的商业合同可约定不同条款。 [许可](LICENSE) · [贡献者条款](CONTRIBUTING.md) · [info@gmwallet.app](mailto:info@gmwallet.app)

---

## 一笔付款如何处理

钱包或支付服务签好交易后提交一次。Broadcaster 将签名原文存入 SQLite，向最多三家独立 RPC 供应商并发广播，再核实哪一笔交易入块。即使节点接收后响应超时，后台仍按原哈希核查，重启后接着处理。

EVM 转账按已提交任务的 Nonce 调度，替换后继续跟踪新旧哈希。Solana 代付由用户和代付方对同一消息签名；当最终确定的区块历史证实 blockhash 已过期且交易未入块，Broadcaster 向提交方报告本次广播失败。后续处理由应用决定。

## 快速开始

### 1. 配置服务

以下示例需要 Go 1.26.2+、OpenSSL、curl 和 jq。在仓库根目录构建服务并复制以太坊示例配置。以下凭证只需生成一次，重启时继续使用同一组。

```sh
go build -o broadcaster ./cmd/broadcaster
cp config.example.json config.json
export BROADCASTER_API_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_ADMIN_TOKEN="$(openssl rand -hex 32)"
export BROADCASTER_STORAGE_KEY="$(openssl rand -base64 32)"
```

要接收回调，在 config.json 的 wallet 客户端填写 HTTPS 接收地址 `webhook`，并为 `webhookSecret` 配置至少 24 字节的独立密钥。将凭证保存在私有部署配置中。 [配置字段](docs/api.zh-CN.md#运行)

### 2. 启用节点并启动服务

将获准在主网发送的签名探针放入 `probes/1.json`。启动时服务核对链并真实广播探针，通过后该节点才能处理业务交易。也可在启动后用管理令牌登记节点。 [探针格式与节点准入](docs/api.zh-CN.md#rpc-节点管理)

```sh
./broadcaster -config config.json -db broadcaster.db
```

### 3. 提交已签名转账

在另一个终端加载同一业务令牌，将十六进制签名原文放入 `SIGNED_TX`。把返回的哈希和对应付款单一起保存。

```sh
BASE=http://127.0.0.1:8080
TX_HASH=$(jq -n --arg raw "$SIGNED_TX" '{rawTransaction: $raw}' |
  curl -fsS "$BASE/EVM/transactions?chainId=1" \
    -H "Authorization: Bearer $BROADCASTER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -er '.txHash')
```

## 接收交易结果

HTTP 202 表示任务已保存。配置回调后，任务结束时原提交服务会收到已签名的 `transaction.closed` 事件。根据任务结果与执行状态更新订单。

| 可见状态 | 业务含义 |
|---|---|
| `CONFIRMING` + `PENDING` | 已看到入块，正在核实执行结果，付款继续显示「确认中」。 |
| `INCLUDED` + `SUCCESS` | 已核实执行成功。需要更多确认数的业务继续按自己的结算规则处理。 |
| `INCLUDED` + `FAILED` | 已入块但执行失败，从 `reason` 和 `evidence` 读取原因。 |
| `EXPIRED` + `BLOCKHASH_EXPIRED` | Solana 交易过期且未入块，提交方收到 `transaction.closed` 失败通知。 |
| `EXPIRED` + `TRACKING_DEADLINE` | 本地跟踪到期，再次付款前先核对原任务。 |
| `REPLACED_EXTERNALLY` | 外部交易已占用 Nonce，根据占用该 Nonce 的交易哈希对账。 |

接收方验证回调签名，持久化事件，并按 `eventId` 去重。需要补查时，用提交时返回的哈希查询。 [回调格式、重试与乱序](docs/api.zh-CN.md#故障恢复与上游事件)

```sh
curl -fsS "$BASE/EVM/transactions/$TX_HASH?chainId=1" \
  -H "Authorization: Bearer $BROADCASTER_API_TOKEN" |
  jq '{taskState, taskOutcome, executionStatus, reason}'
```

## 支持的网络

| 主网 | 提交入口 |
|---|---|
| Ethereum · Arbitrum One · BNB Smart Chain · Avalanche C-Chain · Polygon PoS · OP Mainnet · Base · HyperEVM · Monad · Arc · Plasma · Robinhood Chain | `POST /EVM/transactions` |
| Solana | `POST /solana/transactions` |
| Bitcoin · TRON · Aptos · Sui · TON | `POST /networks/{network}/transactions` |

每条主网内置一家供应商。15 条使用 PublicNode，Monad 使用 Monad Foundation，Plasma 使用官方公共 RPC，TON 使用 TON Center。生产环境可私下登记其他独立供应商。调度器优先选择标有 MEV-Protected 的路由；同时发往普通交易池的那一份仍会公开。 [18 链配置](config.mainnets.json) · [节点目录与限制](docs/api.zh-CN.md#rpc-节点管理)

## 接入文档

- [EVM 提交、Nonce、强制广播与替换](docs/api.zh-CN.md#evm-调用方接口)
- [Solana 代付和五条原生链的签名格式](docs/api.zh-CN.md#非-evm-调用方接口)
- [任务状态、执行失败与恢复](docs/api.zh-CN.md#evm-交易生命周期)
- [节点准入、熔断与部署](docs/api.zh-CN.md#rpc-节点管理)
