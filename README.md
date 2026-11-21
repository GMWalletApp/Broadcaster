# Broadcaster

GM Wallet 的多链 EVM 已签名交易广播与生命周期管理服务。调用方提交一次，服务负责持久化、按账户 Nonce 调度、向最多三家独立付费 RPC 供应商并发广播、核实链上结果并通知上游。本文是首版开发契约。仓库当前处于设计阶段。

签名方自行查询和选取 Nonce、构造交易并签名，也负责签署加价替换版本。Broadcaster 从已签名原文恢复外层发送地址和 Nonce，按账户调度已接收任务。用户仍可持私钥在其他钱包发交易。Broadcaster 处理普通 EVM 交易和 EIP-7702 Type 4 交易，包括逐条授权诊断。独立的 ERC-4337 Bundler 接收并打包 UserOperation；Broadcaster 接收 Bundler 签好的外层交易，负责多供应商广播和入块通知。

## 调用方接口

交易提交和管理操作需要鉴权，查询和变更限定在调用方有权访问的任务或账户内。POST 和 PATCH 的请求体使用 JSON。下表中的 rawTransaction 是完整的十六进制已签名原文。Hash 路径支持用 chainId 查询参数限定网络；出现跨链歧义时必须指定链。

| 方法与路径 | 输入 | 响应与副作用 |
|---|---|---|
| POST /transactions | rawTransaction | 解析签名、链、发送方、外层 Nonce 和本地 Hash。首次持久化返回 202 及 taskId、txHash、state。重复提交相同 Hash 返回 200 及已有任务。同链、同发送方、同 Nonce 已由不同 Hash 登记时返回 409 NONCE_CONFLICT。 |
| GET /transactions/{hash} | 原始或替换 Hash | 返回 200，包含任务阶段和结果、所查 Hash 的状态、当前调度版本、全部候选、各轮各节点结果、链上证据及 7702 诊断。未知或不可见的 Hash 返回 404。 |
| POST /transactions/{hash}/force | reason | 对当前可调度版本立即安排一轮广播，记录操作者、时间和原因，返回 202。只跳过本地 Nonce 排序，仍受节点冷却、可用性及链上状态约束。同一轮已排队或在途时复用该轮。 |
| POST /transactions/{hash}/replacement | rawTransaction | 用路径指定待替换的当前版本。新签名交易通过校验，且 chainId、sender、nonce 相同后，在原任务下登记新 Hash，返回 202。重复提交同一已登记替换版本返回 200。原 Hash 继续接受链上监控。 |
| GET /accounts/{address}/queue?chainId=… | 地址、目标链 | 按外层 Nonce 升序返回未关闭任务、当前版本、排队原因和下次调度时间。队列只列已接收的任务。 |
| GET /accounts/{address}/nonce?chainId=… | 地址、目标链，可选 blockTag（latest 或 pending） | 直接向该链健康 RPC 调用 eth_getTransactionCount。默认 latest；200 返回 chainId、address、blockTag、nonce（十进制字符串）。RPC 均不可用时返回 503 NONCE_VIEW_UNAVAILABLE。 |

**冲突与错误。** 原文无法解码、签名无效、链未启用或整笔交易违反协议规则时，提交返回 4xx 和具体原因，校验失败的请求不占用 Nonce。已有 Hash 的幂等查询优先于依赖当前链状态的预检，先校验调用方访问权限。任务表唯一键为 (chainId, sender, nonce)，签名交易以 (chainId, txHash) 去重。Hash 幂等检查与新任务插入在同一事务内完成，唯一键冲突时重新读取已提交的任务作判定。并发 replacement 使用当前版本的条件更新；对过时版本提交不同的新 Hash、在核查 Nonce 消耗者或已关闭时尝试替换，以及对不可调度版本执行 force，均返回 409 和原因。已关闭任务保留提交历史及终止结果。

**提交响应示例**（标识符省略中间字符）：

~~~json
{"taskId":"task_42","chainId":"1","txHash":"0x…","state":"QUEUED"}
~~~

202 表示服务已持久化任务。调用方可用返回的 Hash 查询进度；自动重播、查回执和故障恢复无需调用 retry 接口。

## Nonce 查询与任务排序

签名服务可以直接向自己的 RPC 查询，也可以调用 Broadcaster 的只读接口。两者使用相同的 RPC 方法。例如，GET /accounts/{R}/nonce?chainId=1 对应 eth_getTransactionCount(R, "latest")，返回最近区块状态中的账户 Nonce。选择 pending 时返回该节点考虑本地 pending 交易后的视图，各供应商可能不同。每次查询均读 RPC，不叠加本地排队任务，不产生预留或写入账户计数器。[Ethereum JSON-RPC](https://ethereum.org/developers/docs/apis/json-rpc/#eth_gettransactioncount)

~~~mermaid
flowchart TD
    Read["签名方查自己的 RPC 或 GET nonce"] --> Sign["选取外层发送账户的 Nonce，签名"]
    Sign --> Submit["POST /transactions：原子登记任务"]
    Submit -->|相同 Hash| Existing["返回已有任务"]
    Submit -->|不同 Hash 且任务键冲突| Conflict["409 NONCE_CONFLICT，本次提交不入队"]
    Submit -->|新任务| Queue["按链、外层发送方和 Nonce 排队"]
    Conflict -->|确需替换原任务| Replace["有权限的调用方显式提交 replacement"]
    Replace --> Queue
~~~

数据库保存签名原文及任务键 (chainId, sender, nonce)，用于并发判重、排序和重启恢复。账户的实时 Nonce 始终通过 RPC 读取。两个服务可同时查到 20，也可能在本地任务尚未传播时都从 pending 读到 20。不同交易使用同一个 20 时只有一个普通提交能创建任务。另一笔若是独立付款，由签名方核对在途交易和队列，另选可用值重新签名；只有同一任务的有意替换才用 replacement。

发现外层账户的链上 Nonce 已越过本地任务时，逐笔核查候选 Hash。自己的版本入块则正常关闭；证实外部消耗则通知原提交方并重新调度剩余任务；证据不足则暂停该任务重播并继续核查。新区块、ChainScanner 地址活动或节点返回 nonce too low 均可触发核查。查询接口始终可独立从 RPC 读取，无需等待扫描进度。

## 付费 RPC 节点管理

每个端点归属于一个稳定的 providerId 和一条链。同一供应商的多个域名、密钥或端点只占一路。每条接入链使用同一个自购节点池，**所有入池端点必须能广播普通 EVM 交易、EIP-7702 Type 4 交易，以及 ERC-4337 Bundler 签好的外层交易**。4337 外层交易调用目标链的 EntryPoint 合约，经 eth_sendRawTransaction 提交。面向钱包的 eth_sendUserOperation 由独立 Bundler 提供。

| 方法与路径 | 行为 |
|---|---|
| POST /rpc-providers | 管理员提交 providerId、chainId、端点和凭证配置。验证实际 chainId、鉴权、查询与广播权限、普通交易和 Type 4 广播能力，以及对已配置 EntryPoint 的 4337 外层交易广播权限。全部通过才登记启用并返回 201。失败或能力无法证实时返回具体检查项与原因。 |
| GET /rpc-providers?chainId=… | 返回供应商和端点标识、启停状态、最近高度及观测时间、读和广播健康状况、失败计数、冷却截止时间与最近错误。凭证与 URL 中的密钥须脱敏。 |
| PATCH /rpc-providers/{id} | 以端点记录 ID 修改配置或启停。端点、凭证或链变更时先重新准入，检查通过后原子切换；失败时原配置继续生效。停用后停止分配新请求。 |

准入须有目标端点和对应凭证的能力证据。例如可使用运维提供的受控已签名测试交易，验证对应写接口接受该类型；费用及链上副作用须在准入测试安排中明确。对 4337，按目标链登记 Bundler 使用的 EntryPoint 地址和版本，核对合约部署，并验证执行端点允许发送有效的、调用该 EntryPoint 的已签名 bundle。查高度成功、能读取历史 Type 4 交易或收到通用参数错误，均不足以证明该端点有 Type 4 广播权限。

### 每轮选点

按链筛选已启用、高度检查可用且广播未冷却的供应商。每家选一个端点，同轮立即发送同一份签名原文。三家或以上发三家，两家发两家，一家即发一家。零家可用时保留调度请求，记录 NO_AVAILABLE_PROVIDER，最早可试发的端点恢复时唤醒。已广播的候选继续监控。第一家接收后，其他已发出的请求照常收集结果。

### 高度检查、广播熔断与恢复

使用 eth_blockNumber 记录端点延迟、最后高度和最近推进时间，并对比同链独立供应商。单次高度持平是正常现象；持续落后或停止推进按链的出块节奏判定。只有一家时参考该端点自身近期推进情况。高度检查和广播各自记录健康状态，高度探测成功只能恢复读侧健康。[eth_blockNumber](https://eips.ethereum.org/EIPS/eip-1474#eth_blocknumber)

下表是**每条链、每个端点的广播熔断器**。冷却阶梯为 3、6、10、15、21、28、36、45、55、60 分钟，之后保持 60 分钟。每档都需要三次端点故障；首次 3 分钟是 Broadcaster 的约定。阶梯后段及上限依据 [ChainScanner 的计数实现](https://github.com/GMWalletApp/ChainScanner/blob/87d802f3daa946e265ed8e396378fb13884a5eee/internal/nodes/source_pool.go#L57-L72)。

| 当前状态 | 条件 | 下一状态和动作 |
|---|---|---|
| HEALTHY | 收到可归因于该端点的超时、断连、限流、凭证失效或服务端故障 | 前两次保持 HEALTHY 并累计；第 3 次进入 COOLDOWN，记录截止时间，当前档计数归零。 |
| COOLDOWN | 未到期 | 跳过新广播；保留每条在途请求的真实结果。过期轮次的迟到响应不能提前解除冷却。 |
| COOLDOWN | 已到期且有待发送交易 | 进入 HALF_OPEN，跨实例只放行一个实际广播试发。无交易时等待下次调度。 |
| HALF_OPEN | 单次试发发生端点故障 | 前两次保持 HALF_OPEN，每次只放行一个试发；第 3 次进入下一档 COOLDOWN。 |
| HEALTHY / HALF_OPEN | 接收并返回预期 Hash，或明确返回 already known | 进入 HEALTHY，清零失败次数和冷却档位。 |
| HEALTHY / HALF_OPEN | 余额不足、替换加价不足、nonce too low 等交易级拒绝 | 记录到该 Hash，不增减端点故障计数；半开时释放试发名额。nonce too low 触发链上 Nonce 核查。 |

计数、档位、截止时间及半开试发租约需跨实例共享，按端点原子更新。三家并发各计各的。一家返回预期 Hash，另一家返回 already known，第三家超时，本轮交易即可进入 PENDING，只有第三家增加一次故障。already known 表示该节点已知这笔交易。[Geth 交易池错误定义](https://github.com/ethereum/go-ethereum/blob/master/core/txpool/errors.go)

## 交易生命周期

**结束条件是候选 Hash 已出现在观测时的规范区块中。** 保存交易 Hash、区块号、区块 Hash、交易索引和观测时间。可通过区块交易列表或与规范区块核对过的回执建立入块证据；只有 RPC 接收或 pending 交易对象时继续跟踪。入块即结束广播任务，关闭后不追踪区块重组，也不承诺链级最终性。

任务汇总一个业务提交及其替换版本；签名交易记录每个 Hash；广播记录保存某 Hash 向一家供应商的一次请求。接口分别返回 taskState、taskOutcome、executionStatus。入块结果为 INCLUDED。回执 status 为 1 时 executionStatus 为 SUCCESS，0 为 FAILED，暂缺回执则为 UNKNOWN。查询可在取得同一区块的回执后补充执行结果。回执失败的交易同样已入块。[EIP-658 回执状态](https://eips.ethereum.org/EIPS/eip-658)

~~~mermaid
flowchart TD
    Submit["提交签名原文，按任务唯一键判重"] --> Q["QUEUED"]
    Q -->|具备调度条件| T["TRACKING：最多三家广播，监听所有候选"]
    T -->|显式 replacement| T
    Q -->|发现 Nonce 已越过| R["RESOLVING_NONCE：暂停该槽重播"]
    T -->|发现 Nonce 已越过| R
    R -->|重新核实该值仍可用| T
    Q -->|候选已在块内| I["CLOSED / INCLUDED"]
    T -->|候选已在块内| I
    R -->|本任务任一候选在块内| I
    R -->|证实外部消耗| X["CLOSED / REPLACED_EXTERNALLY"]
    X --> N["通知提交方，重排该账户的剩余任务"]
    I --> A["通知提交方，唤醒该账户后续任务"]
    Q --> E["永久拒绝或期限到达时关闭"]
    T --> E
    R -->|期限到达| E
~~~

### 任务状态转换

| 当前状态 | 转换条件 | 下一状态与动作 |
|---|---|---|
| 尚无任务 | 首次提交通过校验，已持久化原文、任务键和调度信息 | QUEUED。重复 Hash 返回原任务；本地任务键冲突返回 409 NONCE_CONFLICT；已核实链上越过提交值，且非本任务候选入块时返回 409 NONCE_CONSUMED。 |
| QUEUED | 账户排序允许（或有效 force），且至少一家可发送 | TRACKING，发送首轮。缺前序 Nonce 或没有可用节点时保持 QUEUED，记录原因。 |
| TRACKING | RPC 返回接收、拒绝、超时，或签名方提供替换交易 | 保持 TRACKING，更新对应 Hash 和节点记录。替换版本开始调度，旧版本停止主动重播，仍参与入块核查。 |
| QUEUED / TRACKING | 规范链账户 Nonce 已大于本地交易 Nonce，尚未核实消耗者 | RESOLVING_NONCE。暂停该任务调度；查所有候选及区块。 |
| RESOLVING_NONCE | 证据不足，或复核发现此前读数已失效 | 证据不足保持原状态；若目标值仍可用，回 TRACKING，按正常排序和选点约束恢复。 |
| 任意未关闭状态 | 在当前规范区块核实任务内某个 Hash | CLOSED / INCLUDED，记录 winningHash 及独立执行结果，其他候选记 REPLACED。通知上游并更新账户队列。 |
| RESOLVING_NONCE | 定位到任务外交易消耗该 Nonce（含已生效的 7702 授权） | CLOSED / REPLACED_EXTERNALLY。返回错误 NONCE_CONSUMED_EXTERNALLY、受影响的本地 Nonce 和 Hash、消耗者及区块，停止全部候选。通知原提交方并重排队列。 |
| 未关闭状态 | 所有候选永久无效且证据充分 / 配置的追踪期限到达 | 分别以 CLOSED / REJECTED 或 CLOSED / EXPIRED 结束。后者保留最后观测，已发原文仍可能上链。 |

已接收任务的外部覆盖通过 GET 任务结果和异步事件报告。核查期间查询可见 RESOLVING_NONCE；单个 RPC 拒绝、空回执、链上 Nonce 增长都不能单独证明“外部覆盖”。已绑定 Hash 的本地任务历史可追溯；新的签名交易不得复用被链上消耗的槽位。

### 签名交易与节点请求

| 当前 Hash 状态 | 条件 | 下一状态与动作 |
|---|---|---|
| QUEUED | 被选为当前版本且可发送 | BROADCASTING。持久化轮次再并发发送；未满足条件继续排队。 |
| BROADCASTING | 一家接收或明确已知该 Hash | PENDING，继续收集同轮其他结果。 |
| BROADCASTING | 至少一路结果不明且无明确接收 | SUBMISSION_UNKNOWN。按本地 Hash 核查，避免在超时后误报失败。 |
| BROADCASTING | 全部明确拒绝 | 可恢复错误回 QUEUED，记录退避或阻塞原因；提示 Nonce 过低则触发账户对账；永久原文错误记 REJECTED。 |
| SUBMISSION_UNKNOWN | 查到 pending / 暂不可见且 Nonce 可用 | 前者转 PENDING；后者当前版本按策略回 BROADCASTING，旧版本保持观察。 |
| PENDING | 仍未入块 | 保持 PENDING。可重播同一原文；费用不足时通知签名方提供 replacement。新一轮超时保留先前接收证据。 |
| 任意仍可能入块的候选 | 本 Hash 入块 / 同任务其他 Hash 入块 / 外部消耗得到核实 | 分别落为 INCLUDED / REPLACED / REPLACED_EXTERNALLY。任务同时关闭。 |
| 任意仍在追踪的候选 | 任务到期 | 停止主动调度和监控，保留最后观测。 |

每条节点请求从 IN_FLIGHT 转为 ACCEPTED、ALREADY_KNOWN、REJECTED 或 UNKNOWN。记录轮次、端点、供应商、时间、延迟、错误分类及脱敏原始错误。迟到响应只补充节点记录，已关闭任务保持原结果。

按任一候选 Hash 查询，返回 taskId、taskState、taskOutcome、chainId、sender、nonce、queriedHash、activeHash、winningHash、consumingHash、attempts 及阻塞原因。入块附 completionBasis=BLOCK_INCLUSION、区块证据、executionStatus、receipt 和逐条 authorizations。外部覆盖时返回错误及链上证据。实时账户 Nonce 可另行查询。

### ERC-4337 广播与查询

| 调用方 → 服务 | 接口与标识 | 职责 |
|---|---|---|
| 钱包 → Bundler | eth_supportedEntryPoints、eth_sendUserOperation；返回 userOpHash | 钱包选择该链上 Bundler 支持的 EntryPoint，提交签名的 UserOperation。Bundler 校验、模拟并打包。 |
| Bundler → Broadcaster | POST /transactions，传外层 rawTransaction；返回 taskId、txHash | Bundler 可先查询**外层签名账户**的 Nonce，再签署调用 EntryPoint.handleOps 的交易。沿用同一套最多三家并发、自动重播、显式替换和入块即关闭规则。携带新 7702 授权的 bundle 使用 Type 4 并执行逐条授权诊断。 |
| Bundler → Broadcaster | GET /transactions/{hash} 或入块通知 | Bundler 保存 userOpHash 到外层任务的关联，按 winningHash 定位实际入块版本。若发生替换或重新打包，以实际入块交易中可核实的操作为准。 |
| 钱包 → Bundler | eth_getUserOperationReceipt(userOpHash) | 按 EntryPoint 事件核实指定操作是否入块，通过单条操作回执的 success 报告执行结果。一个 bundle 内各操作可能有不同结果。 |

在 ERC-4337 路径中，Bundler 自行选定外层发送账户的交易 Nonce；随包提供新 7702 授权时，授权账户的签名规则见下节。智能账户的 UserOperation Nonce 由对应 EntryPoint 按账户和 nonce key 管理，签名前可调用 EntryPoint.getNonce(sender, key)。外层交易 CLOSED / INCLUDED 后，Bundler 仍须按 userOpHash 核实每笔操作；外层回执成功不能证明每笔操作成功。[ERC-4337](https://eips.ethereum.org/EIPS/eip-4337) · [ERC-7769 Bundler RPC](https://eips.ethereum.org/EIPS/eip-7769)

### EIP-7702 委托与后续 Gas 代付

设 A 是用户地址，R 是 GM 专用代付地址，D 是 GM 的委托实现合约。以下描述 R 与 A 不同、GM 直接代付的路径。

| 场景 | 用户 A 签什么 | 谁用哪个 Nonce |
|---|---|---|
| 首次设置或变更委托 | A 签署指向 D 的 7702 授权，R 将授权装入 Type 4 交易并签署外层交易。 | 授权使用 **A 的链上账户 Nonce**，外层交易使用 **R 的链上账户 Nonce**。A 自己从外部钱包发出的普通交易也消耗 A 的账户 Nonce。 |
| 委托已生效，后续代付转账 | A 按 D 的规则签署本次转账指令，绑定目标、调用数据、金额及防重放凭据。R 签署发往 **A** 的外层交易并支付 Gas，链在 A 的账户上下文执行 D 的代码。 | 外层仍使用 **R 的链上账户 Nonce**。指令使用 D 为 A 定义的防重放机制；若采用顺序操作 Nonce，同一用户的并发签名须协调这个编号。沿用现有委托时无需再次签署 7702 授权。 |

**首次授权。** 签名方查询 A 的账户 Nonce 来签署授权，查询 R 的账户 Nonce 来签署外层交易。Broadcaster 只用 R 的外层 Nonce 作为任务排序和冲突键，从签名原文中的授权列表诊断 A 的授权。若 A 的外部转账抢先入块，通知签名方核查授权；已经发出的 R 交易继续按其 Hash 跟踪。

**后续代付的调用顺序。**

1. 代付服务查询 R 的账户 Nonce，并根据自己的在途交易选值。A 为本次操作签署 D 要求的指令。
2. 代付服务构造并签署 R 的外层交易，保存业务请求与已签名原文、Hash 的关联，调用 POST /transactions。Broadcaster 在 R 的队列中按外层 Nonce 调度。
3. 提交响应超时则重交相同原文或查询原 Hash。另一笔付款误用相同 Nonce 时，普通提交返回 409；只有需要替换已接收任务时才提交同 Nonce 的签名替换版本。

例如，代付签名方为 R 的两笔付款选定 `100` 和 `101`。如果两笔都属于 A，而 D 对 A 采用顺序操作编号，签名流程还要分别协调 `7` 和 `8`，并保持执行顺序。A 用外部钱包普通转账只会推进 A 的链上账户 Nonce；已签操作仍可能因余额变化而执行失败。实际操作编号与签名字段以所部署的 D 为准。[EIP-7702 授权处理](https://eips.ethereum.org/EIPS/eip-7702#behavior) · [委托持久性](https://eips.ethereum.org/EIPS/eip-7702#persistence-of-code-delegation) · [签名与防重放](https://eips.ethereum.org/EIPS/eip-7702#security-considerations)

### EIP-7702 诊断

1. **提交时**区分整笔无效和个别授权可能被跳过。空授权列表等整笔格式错误直接返回；逐条预检保留索引、委托目标、可恢复的授权者地址、签名 Nonce、原因及所用区块。按协议顺序计算外层发送者 Nonce 的自增，以及同一列表里前序有效授权的影响。
2. **执行时**分别报告 RPC 广播、外层回执和每条授权结果。使用交易在区块中的实际位置重放或等价证据，才能确定该条 APPLIED 或 SKIPPED 及原因。只有预检、标准回执或区块末账户代码时，对无法证明的条目返回 UNKNOWN，并保留预测及证据来源。
3. **记录结果时**绑定观测到的区块 Hash。外层 status=1 时部分授权仍可能被跳过；外层执行回退时已应用的委托仍可保留。[EIP-7702 协议](https://eips.ethereum.org/EIPS/eip-7702#behavior)

## 故障恢复与上游事件

接收事务写入任务、签名原文和调度信息后再响应。工作实例以租约领取到期任务；重启后接手失效租约，先核查全部候选 Hash，再决定按入块关闭、等待或重播。实际 RPC 调用允许重复，数据库对任务、候选和事件去重。Redis 可用于加速，PostgreSQL 保留恢复所需的原文、状态与证据。

状态变更和待发事件通过同一事务写入 outbox。消息队列或 Webhook 至少一次投递，失败可重试，消费方按 eventId 去重并按任务版本处理乱序。transaction.submitted 表示已持久化，入块时 transaction.included 和 transaction.closed 携带入块证据及独立执行状态，其他终止路径的 closed 携带对应原因，transaction.replacement_required 通知签名方加价。任务被外部覆盖时发送 transaction.nonce_conflict 及 transaction.closed，携带 taskId、本地 Hash、链、账户、Nonce、consumingHash、区块和错误码。鉴权按接收方隔离。通知投递失败只重试通知。

## 首版验收示例

| 输入或故障 | 预期可观察结果 |
|---|---|
| 同一 Nonce 并发提交两份不同原文，随后重试胜出的原文 | 一个任务、一个 409；重试返回原任务。只有 replacement 能登记第二个 Hash。 |
| 三家分别返回预期 Hash、already known、超时 | 一笔 PENDING 交易、三条广播记录，只有超时端点增加失败计数。 |
| 某端点连续三次超时；3 分钟内读高度正常；到期后三次试发再超时 | 广播先冷却 3 分钟，再冷却 6 分钟。读请求不重置广播计数。随后一次试发明确接收即可复位。 |
| 可用供应商从三家降到两家、一家、零家 | 实际发出 3、2、1、0 路。零家时任务可恢复，端点可试发后自动唤醒。 |
| 全部请求超时且进程重启，随后查到已上链回执 | 复用原任务，由 SUBMISSION_UNKNOWN 进入 INCLUDED，以观测到的入块证据关闭。 |
| A 广播后显式替换为 B，先在规范区块找到 A，外层回执为 0 | 两个 Hash 均返回 CLOSED / INCLUDED、winningHash=A、executionStatus=FAILED，B 为 REPLACED。关闭后无重组恢复任务。 |
| Type 4 外层成功但一条授权被跳过 | 外层 SUCCESS；能取得交易级证据时列出被跳过的条目和原因，证据不足时报告 UNKNOWN。 |
| 用户自行消耗队列中某 Nonce，节点最初只报 nonce too low | 先核查全部候选。找到外部交易后原任务和通知均带冲突原因，重排剩余任务，较高的已签名交易保持原 Nonce。 |
| 4337 bundle 包含两笔操作，外层入块且执行成功，其中一笔操作执行回退 | Broadcaster 返回外层 INCLUDED 和 winningHash；Bundler 按两个 userOpHash 分别返回 success=true 和 success=false。 |
| 首次代付前，A 在外部钱包先花掉授权 Nonce | 标记 A 的授权需核查或重签；已经发出的 R 交易仍按外层 Hash 独立跟踪。 |
| 同一 R 并发代付两笔，第一笔提交响应超时 | 两笔使用不同的 R 账户 Nonce；重试第一笔复用其原文和 Hash。若同属 A 且 D 采用顺序操作 Nonce，还需验证两份指令各有独立、顺序可执行的编号。 |
| 代付地址 R 的 RPC latest 返回 20，本地队列已有尚未广播的 20 | 查询仍返回 20。另一个 Hash 普通提交同一个 20 时返回 409；查询和冲突均不改变任何账户计数器。 |
