# Broadcaster

GM Wallet 的多链 EVM 已签名交易广播与生命周期管理服务。调用方提交一次，服务负责持久化、按账户 Nonce 调度、向最多三家独立付费 RPC 供应商并发广播、核实链上结果并通知上游。本文是首版开发契约。仓库当前处于设计阶段。

签名方构造交易、分配 Nonce、签名以及签署加价替换交易。Broadcaster 处理普通 EVM 交易和 EIP-7702 Type 4 交易，包括逐条授权诊断。独立的 ERC-4337 Bundler 管理 UserOperation，可将已签名的外层链上交易交给本服务。

## 调用方接口

交易提交和管理操作需要鉴权，查询和变更限定在调用方有权访问的任务或账户内。POST 和 PATCH 的请求体使用 JSON。下表中的 rawTransaction 是完整的十六进制已签名原文。Hash 路径支持用 chainId 查询参数限定网络；出现跨链歧义时必须指定链。

| 方法与路径 | 输入 | 响应与副作用 |
|---|---|---|
| POST /transactions | rawTransaction | 解析签名、链、发送方、外层 Nonce 和本地 Hash。首次持久化返回 202 及 taskId、txHash、state。重复提交相同 Hash 返回 200 及已有任务。同链、同发送方、同 Nonce 已由不同 Hash 登记时返回 409 NONCE_CONFLICT。 |
| GET /transactions/{hash} | 原始或替换 Hash | 返回 200，包含任务阶段和结果、所查 Hash 的状态、当前调度版本、全部候选、各轮各节点结果、链上证据及 7702 诊断。未知或不可见的 Hash 返回 404。 |
| POST /transactions/{hash}/force | reason | 对当前可调度版本立即安排一轮广播，记录操作者、时间和原因，返回 202。只跳过本地 Nonce 排序，仍受节点冷却、可用性及链上状态约束。同一轮已排队或在途时复用该轮。 |
| POST /transactions/{hash}/replacement | rawTransaction | 用路径指定待替换的当前版本。新签名交易通过校验，且 chainId、sender、nonce 相同后，在原任务下登记新 Hash，返回 202。重复提交同一已登记替换版本返回 200。原 Hash 继续接受链上监控。 |
| GET /accounts/{address}/queue?chainId=… | 地址、目标链 | 按外层 Nonce 升序返回未关闭任务、当前版本、排队原因和下次调度时间。已上链待确认的项注明确认进度。 |

**冲突与错误。** 原文无法解码、签名无效、链未启用或整笔交易违反协议规则时，提交返回 4xx 和具体原因，校验失败的请求不占用 Nonce。已有 Hash 的幂等查询优先于依赖当前链状态的预检，先校验调用方访问权限。任务表唯一键为 (chainId, sender, nonce)，签名交易以 (chainId, txHash) 去重。重复提交检查与占位在同一事务内完成。并发 replacement 使用当前版本的条件更新；对过时版本提交不同的新 Hash、在确认中或已关闭时尝试替换，以及对不可调度版本执行 force，均返回 409 和原因。到期关闭仍保留占位与历史。

**提交响应示例**（标识符省略中间字符）：

~~~json
{"taskId":"task_42","chainId":"1","txHash":"0x…","state":"QUEUED"}
~~~

202 表示服务已持久化任务。调用方可用返回的 Hash 查询进度；自动重播、查回执和故障恢复无需调用 retry 接口。

## 付费 RPC 节点管理

每个端点归属于一个稳定的 providerId 和一条链。同一供应商的多个域名、密钥或端点只占一路。每条接入链使用同一个自购节点池，**所有入池端点必须支持普通交易和 EIP-7702 Type 4 广播**。

| 方法与路径 | 行为 |
|---|---|
| POST /rpc-providers | 管理员提交 providerId、chainId、端点和凭证配置。验证实际 chainId、鉴权、查询与广播权限、普通交易和 Type 4 广播能力。全部通过才登记启用并返回 201。失败或能力无法证实时返回具体检查项与原因。 |
| GET /rpc-providers?chainId=… | 返回供应商和端点标识、启停状态、最近高度及观测时间、读和广播健康状况、失败计数、冷却截止时间与最近错误。凭证与 URL 中的密钥须脱敏。 |
| PATCH /rpc-providers/{id} | 以端点记录 ID 修改配置或启停。端点、凭证或链变更时先重新准入，检查通过后原子切换；失败时原配置继续生效。停用后停止分配新请求。 |

准入须有目标端点和对应凭证的能力证据。例如可使用运维提供的受控已签名测试交易，验证对应写接口接受该类型；费用及链上副作用须在准入测试安排中明确。查高度成功、能读取历史 Type 4 交易或收到通用参数错误，均不足以证明该端点有 Type 4 广播权限。

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

PostgreSQL 持久化三个对象。**任务**保存同一个业务提交的候选集合和结束策略。**签名交易**保存不可变的原文、Hash 及其独立进度。**广播记录**保存该 Hash 在某轮向某端点的一次请求。重新广播仅新增广播记录；显式替换才在原任务下新增签名交易。外部交易另存为占用 Nonce 的证据。

~~~mermaid
stateDiagram-v2
    [*] --> QUEUED
    QUEUED --> TRACKING : 发起首轮广播
    QUEUED --> RESOLVING_NONCE : Nonce 已被消耗且来源待查
    TRACKING --> RESOLVING_NONCE : Nonce 已被消耗且来源待查
    RESOLVING_NONCE --> TRACKING : 重组后 Nonce 可用
    QUEUED --> CONFIRMING : 已取得规范链证据
    TRACKING --> CONFIRMING : 已取得规范链证据
    RESOLVING_NONCE --> CONFIRMING : 已定位规范链消耗者
    CONFIRMING --> TRACKING : 区块被移出规范链
    CONFIRMING --> CLOSED : 满足确认或最终性条件
    CLOSED --> [*]
~~~

图中展示有链上结果的路径。下表也列出永久拒绝和追踪到期两种关闭路径。

### 任务阶段

| 当前阶段 | 条件 | 下一阶段和动作 |
|---|---|---|
| 尚无任务 | 首次有效提交成功持久化 | QUEUED。按 (chainId, sender) 排队，按外层 Nonce 调度。账户间独立，同一账户可流水线广播。 |
| QUEUED | 本地排序允许或收到有效 force，且至少一家可试发 | TRACKING。持久化广播轮次并启动链上监控。零家可用或尚有本地 Nonce 缺口时保持 QUEUED，显示原因。 |
| QUEUED / TRACKING | 观察到规范链上的账户 Nonce 已越过目标值，所有候选都没有可核实的回执 | RESOLVING_NONCE。暂停盲目重播，查明消耗者及区块。 |
| 任意未关闭阶段 | 已核实任务内某候选上链，或已定位外部消耗者及其规范区块 | CONFIRMING。记录临时上链结论，暂停同 Nonce 下所有主动广播。 |
| RESOLVING_NONCE | 证据仍不足，或因重组该 Nonce 又可用 | 证据不足则保持并重查；若已恢复可用则回 TRACKING，核查全部候选后调度当前版本。 |
| CONFIRMING | 区块仍在规范链但未达结束条件 | 保持 CONFIRMING，更新确认数及链级最终性证据。 |
| CONFIRMING | 核实原区块被重组移出 | 发出 reorged，撤销旧回执、确认数及授权结论，回 TRACKING 并核查全部候选。若在新规范链已找到消耗者，再进入 CONFIRMING。 |
| CONFIRMING | 满足该链为本任务记录的确认深度或最终性条件 | CLOSED，落定 winningHash 或外部消耗者、任务结果和结束依据，写入待发通知。 |
| 任意未关闭阶段 | 已证明所有候选永久无效且没有可能上链的版本，或已到配置的追踪期限 | CLOSED，分别记 REJECTED 或 EXPIRED。保留最后证据；已广播的原文在追踪结束后仍可能上链。 |

TRACKING 阶段对外同时返回下表中的每个 Hash 及其进度。CONFIRMING 时的链上结论可撤销；CLOSED 为本次追踪的终点。按确认深度关闭时返回实际深度，选择链级最终性时持续跟踪到配置的最终性条件。

### 每个签名 Hash 的状态转换

| 当前状态 | 触发条件 | 下一状态 / 动作 |
|---|---|---|
| QUEUED | 被选为主动版本且本轮可发送 | BROADCASTING。先持久化轮次与选定端点，再并发发送。尚未满足条件则留在队列。 |
| BROADCASTING | 任一家确认接收或已知该 Hash | PENDING，继续收集同轮其他结果。 |
| BROADCASTING | 没有接收证据，至少一路超时、响应不确定或发送过程中宕机 | SUBMISSION_UNKNOWN，先用本地 Hash 查交易与回执。 |
| BROADCASTING | 全部明确拒绝 | 暂时性错误 → QUEUED 并设置重试时间；需补足余额或重新签名 → QUEUED 并注明阻塞原因；Nonce 已过的提示 → 核查链上 Nonce；确定原文永久无效 → REJECTED。 |
| SUBMISSION_UNKNOWN | 查到未打包的交易 / 查到规范链回执 | 分别进入 PENDING / INCLUDED。查不到且 Nonce 仍可用时，当前版本按退避安排重播；旧版本继续按 Hash 监控。 |
| PENDING | 尚无规范链回执 | 保持 PENDING。多节点观察不到且 Nonce 可用，可重播当前版本。费用不足时记录 STUCK 并发 replacement_required，等待签名方。单次空回执不构成丢弃证据。 |
| QUEUED / SUBMISSION_UNKNOWN / PENDING | 当前版本需要重播、已解除阻塞，或收到可执行的 force | 新开广播轮次。QUEUED 或 SUBMISSION_UNKNOWN 在发出请求时进入 BROADCASTING；已有接收证据时保留 PENDING，用轮次记录重播进展；迟到的拒绝或超时不能覆盖已取得的证据。 |
| 任意仍可能上链的状态 | 监控取得该 Hash 的规范链回执 | INCLUDED，记录区块 Hash、高度、交易位置、外层执行状态及授权诊断。随即进入 CONFIRMING。可从未知或排队状态直接观察到上链。 |
| CONFIRMING | 未达到结束策略 / 达到配置确认深度 / 达到要求的链级最终性 | 分别保持 CONFIRMING / 以 CONFIRMED 结束 / 以 FINALIZED 结束。回执 status 为 0 也要完成这一步。 |
| INCLUDED / CONFIRMING | 区块被重组移出 | 撤销原纳入证据并记录 reorged。若已有新规范链回执则回 INCLUDED；仅能看到待处理交易则回 PENDING；否则回 SUBMISSION_UNKNOWN 核查。 |
| 任意未落定候选 | 另一个 Hash 或外部交易已按结束策略确定消耗该 Nonce | REPLACED，记录消耗者及依据。仅收到替换请求时保留原版本状态。 |
| 任意仍在追踪的候选 | 任务追踪期限到达 | 停止调度，保留最后一次真实观测，任务结果为 EXPIRED。 |

替换 B 登记后，B 从 QUEUED 开始，A 停止新一轮主动广播。已发出的 A 请求可能仍返回，监控同时查询 A 和 B。服务重启、替换请求和链上观察并发发生时，以任务版本控制调度；任一候选先上链均能进入确认流程。

**Nonce 核查。** 使用规范链上的已执行 Nonce 作证据，区分 pending 视图。定位原交易、同任务替换交易或任务外消耗者后，记录交易和区块，并复核规范链归属。EIP-7702 授权也可推进授权账户 Nonce，核查应覆盖由其他外层发送者携带的授权。无法定位时保持 RESOLVING_NONCE，直至取得证据或到期。

### 广播记录、结束结果与查询字段

每条广播记录从 IN_FLIGHT 转为以下结果之一：ACCEPTED（返回预期 Hash）、ALREADY_KNOWN、REJECTED（明确拒绝）或 UNKNOWN（超时、断连、返回异常或发送结果丢失）。同时保存轮次、供应商、端点、时间、延迟、错误分类及脱敏后的原始错误。广播记录只说明该端点的一次响应。

| 任务结果 | 关闭依据 |
|---|---|
| EXECUTED / REVERTED | 任务内实际消费该 Nonce 的 Hash 达到结束策略，回执 status 分别为 1 / 0。winningHash 指向该版本。 |
| REPLACED_EXTERNALLY | 已核实任务外交易消耗该 Nonce 并达到结束策略。返回 consumingHash 和原因。 |
| REJECTED | 所有候选均有不可恢复的原文错误依据，且无仍可能上链的版本。 |
| EXPIRED | 已达服务追踪期限，仍无足够证据落定结果。返回截止时间和最后观测。 |

GET 结果应提供 taskId、taskState、taskOutcome、chainId、sender、nonce、queriedHash、activeHash、winningHash、consumingHash、attempts，以及阻塞原因和下次调度时间。每个候选包含 state、broadcasts、receipt、confirmations、authorizations。链上结果附 completionBasis（CONFIRMATION_DEPTH 或 CHAIN_FINALITY）、区块 Hash 和实际深度；未确定字段返回 null。外层执行结果只描述该交易，支付入账等业务对账由上游处理。

### EIP-7702 诊断

1. **提交时**区分整笔无效和个别授权可能被跳过。空授权列表等整笔格式错误直接返回；逐条预检保留索引、委托目标、可恢复的授权者地址、签名 Nonce、原因及所用区块。按协议顺序计算外层发送者 Nonce 的自增，以及同一列表里前序有效授权的影响。
2. **执行时**分别报告 RPC 广播、外层回执和每条授权结果。使用交易在区块中的实际位置重放或等价证据，才能确定该条 APPLIED 或 SKIPPED 及原因。只有预检、标准回执或区块末账户代码时，对无法证明的条目返回 UNKNOWN，并保留预测及证据来源。
3. **确认时**绑定区块 Hash。外层 status=1 时部分授权仍可能被跳过；外层执行回退时已应用的委托仍可保留。重组后重新计算。[EIP-7702 协议](https://eips.ethereum.org/EIPS/eip-7702#behavior)

## 故障恢复与上游事件

接收事务写入任务、签名原文和调度信息后再响应。工作实例以租约领取到期任务；重启后接手失效租约，先核查全部候选 Hash，再决定确认、等待或重播。实际 RPC 调用允许重复，数据库对任务、候选和事件去重。Redis 可用于加速，PostgreSQL 保留恢复所需的原文、状态与证据。

状态变更和待发事件通过同一事务写入 outbox。消息队列或 Webhook 至少一次投递，失败可重试，消费方按 eventId 去重并按任务版本处理乱序。submitted 表示已持久化，included 表示目前的规范链已纳入，reorged 撤销相应区块证据，replacement_required 通知签名方加价，closed 携带结束原因、Hash 和依据。事件名统一使用 transaction. 前缀。通知投递失败只重试通知。

## 首版验收示例

| 输入或故障 | 预期可观察结果 |
|---|---|
| 同一 Nonce 并发提交两份不同原文，随后重试胜出的原文 | 一个任务、一个 409；重试返回原任务。只有 replacement 能登记第二个 Hash。 |
| 三家分别返回预期 Hash、already known、超时 | 一笔 PENDING 交易、三条广播记录，只有超时端点增加失败计数。 |
| 某端点连续三次超时；3 分钟内读高度正常；到期后三次试发再超时 | 广播先冷却 3 分钟，再冷却 6 分钟。读请求不重置广播计数。随后一次试发明确接收即可复位。 |
| 可用供应商从三家降到两家、一家、零家 | 实际发出 3、2、1、0 路。零家时任务可恢复，端点可试发后自动唤醒。 |
| 全部请求超时且进程重启，随后查到已上链回执 | 复用原任务，由 SUBMISSION_UNKNOWN 进入 INCLUDED，按链配置完成确认。 |
| A 广播后显式替换为 B，A 先上链后遭重组，最终 B 达标 | A、B 查到同一任务，winningHash 为 B，A 为 REPLACED，旧回执撤销且保留重组记录。 |
| Type 4 外层成功但一条授权被跳过 | 外层 EXECUTED；能取得交易级证据时列出被跳过的条目和原因，证据不足时报告 UNKNOWN。 |
