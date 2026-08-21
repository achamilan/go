gcdeadwindow 设计文档
======================

gcdeadwindow 是 Go runtime 的区间采集调试特性：在一次"窗口"期间跟踪分配
（精确模式 MemProfileRate 置 1，或采样模式置 sampleRate），每个 GC 周期
输出本周期的 alloc/freed 增量和窗口累计存活，并与 GC 日志（gctrace）交织
写入同一报告文件，可生成 Excel 对比报表。

与 gcdeadtrace 会话模式的区别：窗口模式不按 session/goroutine 归属，只回答
"这段时间内分配了多少、死了多少、还剩多少、分别在哪些调用点"。两种模式
完全互斥。

1. 版本日志
=============

v1:  区间采集模式：GcDeadWindowStart/Stop API、per-GC 报告、demo 和 Excel
     报表。[aa02436279]
v2:  gctrace 同步入文件 + GC对比 sheet + MB 量级 demo。[59e39f230f]
v3:  tee 重构为独立函数 gcDeadWindowGCTrace 并移入 gcDeadWindowPrint 内
     （修复强制 GC 时"块→gc行→块"的顺序倒置），mgc.go 恢复上游零 diff。
     Start 强制基线 GC。新增 net/http/pprof HTTP start 端点。
v4:  HTTP 端点改为 profile 式流式响应（连接保持到窗口结束，报告作为响应
     体流回，curl -o 直接落客户端 cwd；服务器侧临时文件读完即删）。
     GcDeadWindowStart 改为返回 bool（拒绝时 false，HTTP 据此返回 409）。
     最终报告门控从 finalPending 布尔改为 finalCycle 周期号门控——修复
     布尔"检查后清除"竞态导致 Stop 后的后续 GC 无限打印尾随块（会重建
     已删除的临时文件）；Stop 的 GC 之后遗留对象的释放不再出现在报告中。
v5:  单块输出改为分块冲刷（超过 1MB 高水位即写盘重置），彻底消除单周期
     报告 8MB 截断（产品环境实测触发 ..TRUNCATED）；gcDeadTraceBufSize
     现在只约束单次写盘大小。8192 站点上限仍在（超出打 warning 行，
     段首汇总计数不受丢弃影响）。
v6:  站点数上限移除：聚合表（raw + sites）改为持久切片，初始 1024、按需
     翻倍（persistentalloc 不释放，废弃一半浪费 ≤2× 终态；站点数天然受
     程序分配调用点总数约束）。droppedSiteCount 与 warning 行移除。
v7:  块头新增 gcdeadwindow:totalalloc 判别行（gcController.totalAlloc
     增量，span 粒度、采样无关）；GC对比 sheet 主口径从 heap 差值公式改为
     totalAlloc Δ（生产 12GB+ 堆服务单周期 sweep 30-79s，heap 差值公式
     严重失真：实测同周期 heap 口径 1372MB vs totalalloc 528MB vs 窗口
     511MB），heap 口径保留为参考列。站点 sheet 拆分为四个分类 sheet。
v8:  gcIntervalSeconds 周期 GC 模式：>0 时 setGCPercent(-1) 关闭原有触发
     （gcpercent<0 同时门控 pacer 堆触发和 forcegc 定时触发，零侵入
     mgc/proc），改由定时 goroutine 每 N 秒强制完整 GC，报告周期等间隔；
     Stop 在最终 GC 之后恢复原 gcpercent。HTTP 加 gcinterval 参数，demo
     加 -gcinterval flag。
v9:  报告生成 CPU 优化（大业务量采集过载的报告侧）：Phase 1 合并从 O(n²)
     线性扫描改哈希表；Phase 2 由"按解析文本再合并"改为 1:1 转换
     （Phase 1 已按完整 PC 元组合并，字符串键合并永不命中）；删除
     O(n²) 插入排序（分块冲刷后无截断，excel 自行排序）；新增进程生命
     周期 PC 符号化缓存。8192 站点实测每 GC 报告 230-500ms → 4-8ms
     （40-70×）。注：块头 gcdeadwindow:arena 统计行曾加入（2026-08-18）
     次日按需求回退。
v10: sampleRate 采样模式：>1 时 MemProfileRate=sampleRate（标准 heap
     profile 泊松采样），分配/sweep 路径开销同比率下降（5M×64B 实测：
     精确模式 72× 基线，rate=512K ≈ 1×）。报告计数为采样原始值（数据
     验证在采样域仍精确），块头加 gcdeadwindow:rate 行；excel 加估算列。
     HTTP 加 samplerate 参数。附带修复：连续两个无定时器裸窗口时 Stop
     的 notewakeup 触发 "notewakeup - double wakeup" fatal（Start 现在
     对两个 note 先 noteclear）。


2. API
========

```go
runtime.GcDeadWindowStart(seconds int, path string, gcIntervalSeconds int, sampleRate int) bool
runtime.GcDeadWindowStop()
```

- 纯 API 控制，无需 GODEBUG。Start 返回是否成功启动（已有窗口/会话活跃时
  拒绝、打 stderr、返回 false）；调用方可忽略返回值。
- `seconds > 0`：定时 goroutine（`go gcDeadWindowTimer` + notetsleepg）到时
  自动 Stop；`seconds <= 0`：运行直到显式 Stop。
- `path` 非空：报告追加写入该文件（CreateFileW + UTF-8→UTF-16，支持中文
  路径）；空串写 stderr。
- `gcIntervalSeconds > 0`：窗口期间关闭原有 GC 触发（堆 pacer + forcegc），
  每 N 秒强制一次完整 GC，报告周期等间隔；`<= 0` 保持原有触发。应用自己
  的显式 runtime.GC() 不受影响。详见 5.5。
- `sampleRate > 1`：采样模式，MemProfileRate 置 sampleRate，S 字节对象以
  ~S/sampleRate 概率被跟踪；`<= 1` 精确模式（每次分配都跟踪）。详见 5.6。
- 约束：同时只能有一个窗口；与 gcdeadtrace 会话互斥（双向检查）。
- 副作用：采集期间 MemProfileRate 置 1 或 sampleRate（Stop 恢复原值）；
  gcIntervalSeconds>0 时 gcpercent 置 -1（Stop 恢复原值）；path 非空时自动
  开启 gctrace（Stop 恢复原值）。

**基线 GC**：Start 在返回前强制一次完整 GC——扫掉窗口前的垃圾作为干净基线，
首个报告块锚定窗口起点，本周期计数器随之清零，后续周期只统计新分配。
**结束 GC**：Stop 强制一次完整 GC，最终报告在 Stop 返回前写出；该 GC 之后
窗口遗留对象的释放不再被报告（final alive 即 Stop 点真实存活，数据验证
alloc == freed + alive 精确成立）。gctrace/gcpercent 原值在这次 GC 之后才恢复。


3. 使用指南
=============

3.1 三种采集方式
-----------------

**(a) HTTP（推荐，生产免改代码）**：`import _ "net/http/pprof"` 后：

```
# 精确模式短窗口（小服务/低分配率）
curl -o gcdeadwindow.log "http://localhost:6060/debug/gcdeadwindow/start?seconds=60"

# 大业务量生产推荐：采样 + 周期 GC
curl -o gcdeadwindow.log "http://localhost:6060/debug/gcdeadwindow/start?seconds=300&gcinterval=60&samplerate=524288"

# 堆增长缓慢、pacer 长期不触发的服务：周期 GC 强制等间隔报告
curl -o gcdeadwindow.log "http://localhost:6060/debug/gcdeadwindow/start?seconds=600&gcinterval=30"
```

**(b) 代码内嵌**：直接调 runtime.GcDeadWindowStart/Stop（见第 2 节）。

**(c) Demo 验证**：`会话内存/demo`：

```
go build -o demo.exe main.go
./demo.exe -mode gcdeadwindow -duration 10s -windowfile output/demo.log \
    [-gcinterval 1] [-samplerate 4096]
```

3.2 参数选择
-------------

| 场景 | 建议参数 | 理由 |
|------|----------|------|
| 大业务量生产服务 | `samplerate=524288` (+ `gcinterval=30~60`) | 采样后开销≈原生（实测 rate=512K 时 ~1.03× 基线）；周期 GC 摊薄报告成本 |
| 堆增长慢、pacer 不触发 | `gcinterval=10~60` | 否则整个窗口可能只有基线/结束两个报告 |
| 小服务/短窗口精确归因 | 默认（精确模式） | 精确模式开销 ~72× 分配路径，仅限低分配率或短时间 |
| 泄漏候选排查 | 任意 + 看 Alive sheet | final alive 即 Stop 点真实存活 |
| 池化/复用优化 | 任意 + 看 FullyDead sheet | 分配后全死的站点是池化候选 |

sampleRate 选取：应远大于典型对象大小（建议 10~100×），估算公式
（scale = 1/(1−e^(−平均大小/rate))）在小对象区间最准；平均大小接近
rate 的站点泊松噪声和均值近似误差都会放大。rate=512K 对 KB 级小对象
churn 是合理默认。

3.3 报表解读
-------------

```
excel_report_window.exe gcdeadwindow.log   # → gcdeadwindow_report.xlsx
```

- **GC对比 sheet**：先看 Verdict——全 OK 说明窗口采集可信（采样模式含泊松
  噪声，30% 容差内正常）。totalAlloc Δ 是全进程真实分配口径。
- **主 sheet 数据验证**：alloc == freed + alive 应精确为 0（采样模式下在
  采样域精确为 0）。
- **Alive sheet**：窗口结束仍存活的站点 = 长生命周期/疑似泄漏，按存活量
  排序；关注存活持续多个窗口仍增长的站点。
- **FullyDead sheet**：分配后全死的 churn 站点 = 池化/复用优化候选。
- 采样模式：各计数为采样原始值（同 rate 等比缩放，站点排序不受影响），
  估算列 Est = 采样对象数 × rate。


4. HTTP 接口
==============

`import _ "net/http/pprof"` 后，与 CPU profile 端点同语义——连接保持到
窗口结束，报告作为响应体流回，客户端直接保存到本地：

```
curl -o gcdeadwindow.log "http://localhost:6060/debug/gcdeadwindow/start?seconds=30&gcinterval=10&samplerate=524288"
```

Query 参数：

- `seconds`：必须为正整数（缺失/非法 → 400）。
- `gcinterval`：可选，周期 GC 间隔秒数；>0 关闭原有 GC 触发改周期强制 GC。
- `samplerate`：可选，采样率（字节/样本）；>1 启用采样模式。

- 响应：200 + `Content-Disposition: attachment; filename="gcdeadwindow.log"`，
  报告含交织的 gctrace 行。
- 并发：HTTP 侧 single-flight（处理中再来请求 → 409）；runtime 拒绝（已有
  窗口/会话活跃）→ 409（依据 GcDeadWindowStart 的 bool 返回值）。
- 服务器侧用临时文件收集（os.CreateTemp），响应发出后删除；最终报告周期
  门控保证 Stop 后的后续 GC 不会再写该文件（不会重建泄漏）。


5. 实现原理
=============

5.1 跟踪机制
-------------

- 复用 `_KindSpecialGcDeadSession` special；窗口对象的 special 携带哨兵
  `sessionID = ^uint64(0)`，与真实会话对象区分。
- `ss.goid` 槽存窗口代数（gen）：每次 Start 递增，旧窗口遗留对象的 free 因
  gen 不匹配被忽略，实现代际隔离。
- memRecord 上 8 个 `gcDeadWin*` 计数器：4 个本周期（每 GC 报告时 Xchg 读清，
  即窗口内部的"stop+start"）+ 4 个窗口累计；alive = cumAllocs − cumFrees。
- 不用会话表/allocBucketRefs，无 per-session/per-goroutine 归属。
- tiny 分配器对象按块计数（memprofile 固有行为）。

5.2 双 GC 触发点与并发
-----------------------

报告在两处触发：

1. `gcMarkDone` 末尾（gcMarkTermination 之后，后台 mark worker 上）：后台 GC
   的唯一报告点；上一轮 sweep 已由 gcStart 的 finishsweep_m 保证完成。
2. `runtime.GC()` 调用者上的 post-sweep 钩子（等本轮 sweep 完，数据更准）。

两触发点可并发执行（调用方钩子在 mark-done 发布后即可运行，能超过 worker 上
gcMarkTermination 的尾部），共享聚合表与输出 buffer，因此：

- `gcDeadWindowPrintLock` 串行化 gcDeadWindowPrint；
- 去重：某钩子发现本周期计数全 0 且 alive 总量未变则跳过（同步 GC 时两个
  钩子常看到相同状态）；
- 报告门控统一走 `gcDeadWindowReportDue()`：窗口活跃 ⇒ 每周期都报；Stop 后
  仅 `memstats.numgc == gcDeadWindowFinalCycle`（Stop 时存 numgc+1）的那一个
  周期可报。**不能用"布尔 + 打印后清除"**：钩子门控在锁外检查、清除在打印
  末尾，下一周期的钩子可在清除前穿过门控，导致 Stop 后无限尾随打印（HTTP
  场景实测重建了已删除的临时文件；且同一最终 GC 的两个钩子只能活一个，可能
  丢掉更准的 post-sweep 块）。周期号单调只读比较，无竞态。

注意：gcMarkTermination 内 `startTheWorldWithSema`（mgc.go:1518）在 gctrace
打印（mgc.go:1575）**之前**，即 gctrace 打印时世界已重启。

5.3 gctrace 同步（tee）
------------------------

窗口带文件启动时自动 `debug.gctrace=1`（原值存 `gcDeadWindowSavedGCTrace`，
Stop 在强制 GC 之后恢复）。独立函数 `gcDeadWindowGCTrace()`（mprof.go）按
标准 gctrace 格式组行（自写 `appendUint`；`itoaDiv(buf,v,0)` 对 v<10 会多出
前导零，不能用）写入报告文件。**mgc.go 保持上游零 diff。**

**顺序保证的演进**：

- v2 把 tee 放在 gcMarkTermination 末尾。但强制 GC 时 post-sweep 钩子在
  mark-done 发布后即可跑（且首次 CreateFileW 慢），可超过 worker 上的 tee，
  实测出现"块→gc行→块"倒置；PrintLock 只能串行不能重排，无法修复。
- v3 把 tee 移入 `gcDeadWindowPrint`：去重判断之后、写块之前，持
  PrintLock，用 `gcDeadWindowLastTeeGC`（uint32，存最近 tee 的 numgc，
  Start 清零）保证每周期恰好一次。无论哪个钩子先跑，都是 gc 行在前、块在
  后。被去重跳过的周期不产生 gc 行（也无块，一致）。

tee 行无 `(checking for goroutine leaks)` 注解（其为 gcMarkTermination 局部
变量），其余字段与标准 gctrace 完全一致，gctrace 解析器可直接读报告文件。

5.4 Windows 文件写入
---------------------

`writeDeadTraceToFile`：CreateFileW（FILE_APPEND_DATA + OPEN_ALWAYS，每次
调用独立 open/write/close）+ 手动 UTF-8→UTF-16 转换（surrogate 处理抄
os_windows.go 控制台写法），支持中文路径；`_CreateFileW` 用
`//go:cgo_import_dynamic` 注册并加入 stdFunction 列表。Unix 侧为普通
O_APPEND 写。

5.5 周期 GC 模式（gcIntervalSeconds）
--------------------------------------

- 关闭原有触发：`setGCPercent(-1)`。gcpercent<0 同时门控堆 pacer
  （gcTriggerHeap）和 forcegc 定时触发（gcTriggerTime，见 gcTrigger.test），
  无需改 mgc/proc。原值存 `gcDeadWindowSavedGCPercent`（sentinel -2，因 -1
  是合法值），Stop 在最终强制 GC 之后恢复。
- 周期触发：`go gcDeadWindowGCIntervalTimer`——独立 note
  （`gcDeadWindowGCNote`，note 单等待者语义不能与窗口 timer 复用），
  notetsleepg 循环 + gen 检查，每拍调 runtime.GC()；Stop notewakeup 终止。
- 应用显式 runtime.GC() 不受 gcpercent=-1 影响（gcTriggerCycle 不被门控）。
- 无分配的空周期被既有去重逻辑跳过（报告中可能出现 GC 缺号，正常）。
- **note 双 wakeup 修复（v10）**：Stop 无条件 notewakeup 两个 note；连续两个
  无定时器裸窗口（seconds=0 且 gcinterval=0）时，第一个 Stop 的 pending
  wakeup 无人消费，第二个 Stop 触发 "notewakeup - double wakeup" fatal
  （此前 demo/HTTP 恰好被 timer goroutine 启动时的 noteclear 掩盖）。修复：
  Start 在 `active.Store(1)` 前对两个 note 先 noteclear。回归测试
  GCDeadWindowNoteReuse。

5.6 采样模式（sampleRate）
---------------------------

- `MemProfileRate = sampleRate`：标准 heap profile 泊松采样，S 字节对象以
  ~S/rate 概率进入 mProf_Malloc（窗口计数 + special 都只对被采样对象），
  分配/sweep 路径开销同比率下降。
- **计数保持采样原始值**：采样对象的 alloc 与其 free 同属一个采样宇宙，
  alive = cumAllocs − cumFrees 与 alloc == freed + alive 在采样域精确成立
  （excel 数据验证不受影响）。
- 窗口 rate 存 `gcDeadWindowSampleRate`：Stop 在最终强制 GC **之前**恢复
  MemProfileRate，最终报告块的 rate 行必须反映窗口自己的 rate。
- 报告标记：每块头打印 `gcdeadwindow:rate: N bytes per sample`（仅 rate>1）。
- 估算口径（excel 展示值）：与 heap profile 完全相同的逆概率估计
  （runtime/pprof scaleHeapSample）：采样是泊松字节流，平均大小 m 的
  对象被采概率 P = 1−e^(−m/rate)，缩放 scale = 1/P，est = 采样值 ×
  scale。m ≪ rate 时退化为 采样对象数 × rate；m ≫ rate 时 scale→1
  （大对象几乎必被采到，原始值即真实值）。均值近似误差：记录/站点内
  对象大小不均匀时按平均大小代入非线性公式有偏差（上游同样如此）。

5.7 报告生成性能（v9）
-----------------------

gcDeadWindowPrint 每 GC 可由两个钩子各触发一次（去重仅对完全无变化的周期
生效，大业务量下≈每 GC 两次全量），历史三个热点及修复：

| 热点 | 原实现 | v9 后 |
|------|--------|-------|
| Phase 1 pcs 元组合并 | O(n²) 线性扫描整数比较 | 开放寻址哈希表 gcDeadWinHash（随 raw 翻倍+rehash，每次打印 memclr） |
| Phase 2 站点合并 | O(raw×site) 字符串比较 | 1:1 转换——Phase 1 已按完整 pcs 元组合并，字符串键合并永不命中 |
| Phase 3 排序 | O(n²) 插入排序 | 删除（分块冲刷后无截断；excel 自行排序） |
| 符号化 | 每帧每周期 findfunc/funcname/funcline | gcDeadWinSymCache 16k 直接映射缓存（PC 语义进程内不变，每 PC 一次） |

8192 站点 A/B 实测：每 GC 报告 230-500ms → 4-8ms（40-70×）。语义微变：PC
不同但解析到同一源码行的条目现在分成两行相同文本（excel 按文本重聚合，
数据验证不受影响）。

5.8 分配路径开销与采样收益
---------------------------

精确模式（MemProfileRate=1）每次分配付出：profBucket 哈希 +
systemstack(setprofilebucket) + 4 共享原子加 + speciallock 全局锁 +
每对象 ~48B special（sweep 时每对象再 4 原子加）。5M×64B 分配实测：

| 模式 | 耗时 | 相对基线 |
|------|------|----------|
| 无窗口 | 111ms | 1× |
| 精确 rate=1 | 8011ms | 72× |
| rate=4096 | 208ms | 1.9× |
| rate=512K | 115ms | ≈1× |

**大业务量务必用采样模式。**（压测程序：会话内存/demo/allocbench，
站点数压测：会话内存/demo/sitestress，均未入库。）


6. 输出格式
=============

每个 GC 周期（未被去重时）输出 gctrace 行 + 报告块：

```
gc 7 @0.015s 1%: 0+0.51+0 ms clock, 0+0/1.0/1.0+0 ms cpu, 31->32->26 MB, 32 MB goal, ... (forced)
=== GC #7 gcdeadwindow gen=1 elapsed=0s ===
gcdeadwindow:totalalloc: 17628160 bytes allocated since last report (104857600 bytes total)
gcdeadwindow:rate: 4096 bytes per sample        ← 仅采样模式
gcdeadwindow:alive: 2105 objs (31012680 bytes) still alive from 6 sites
  main.winSiteLive (main.go:649) < ...: 2000 objs, 16384000 bytes
gcdeadwindow:alloc: 3345 objs (17623752 bytes) allocated this cycle from 5 sites
gcdeadwindow:freed: 1240 objs (3068040 bytes) freed this cycle from 2 sites
```

- totalalloc：runtime 累计分配计数（span 粒度、采样无关）的本周期增量，
  交叉验证窗口 alloc 的全进程覆盖。
- rate：采样模式标记（仅 rate>1 时出现）；其后所有计数为采样原始值。
- alive：窗口累计存活（cumAllocs − cumFrees），精确字节（采样域）。
- alloc/freed：本周期增量（Xchg 清零）。
- 同一 GC 可能有两个块（双钩子）；强制 GC 的 post-sweep 块数据更准。


7. Excel 报表
===============

`excel_report_window.go`（无第三方依赖，手写 XLSX）：

```
go run excel_report_window.go output/gcdeadwindow_demo.log   # → gcdeadwindow_report.xlsx
```

Sheet 构成：

| Sheet | 内容 |
|-------|------|
| Overview | 各文件汇总（窗口数、报告数、alloc/freed/final alive/peak alive） |
| `<name>` | 顶部汇总（采样模式有说明行）+ per-gen Summary + 数据验证 + Per-GC Summary（采样模式附 Est 估算列） |
| `<name> - Alloc/Freed/FullyDead/Alive` | 四个分类站点 sheet（按 (gen, 首帧) 聚合；Alive=泄漏候选，FullyDead=池化候选） |
| `<name> - SteadyFreed` | 稳态交集：每个稳态 GC 周期（排除首尾）都有释放的站点——持续 churn，池化/复用最高优先级；含 Min/Max Objs per GC、Avg/Total Freed MB |
| `<name> - GC对比` | 窗口 alloc/freed vs 全进程分配（totalAlloc Δ），见下 |

**GC对比口径**：

- **主口径 totalAlloc Δ**：块头 totalalloc 行的增量（span 粒度、采样无关、
  不受 sweep 时滞影响）。窗口 alloc 含 span 空闲槽位天然低 10~30%，Verdict
  只看 alloc 侧：|窗口 alloc − totalAlloc Δ| ≤ max(2MB, 30%×totalAllocΔ)。
- 参考列 `GC Alloc MB (heap口径) = heap1[N] − heap2[N−1]`：大堆 + sweep 滞后
  服务上严重失真（生产实测 37MB 真实分配算出 1424MB），仅作参考。
- `GC Freed MB = heap1[N] − heap2[N]`：本 GC 清扫出的垃圾 ≈ 本周期全进程释放。
- 解析侧必须**合并同 gcNum 的连续块**（双钩子对同一 GC 各打一块，gctrace
  只有一行），否则第二块用已更新的 prevHeap2 算出虚假 GC Alloc 污染累计列。
- 采样模式：窗口 Alloc/Freed MB 列自动改用估算值（采样对象数 × rate），
  原始采样计数见主 sheet Per-GC Summary。

已知系统性差异来源：

1. MB 截断：gctrace 堆值每值 ±1MB；
2. freed 差值含窗口前旧对象的垃圾（GC freed 全进程，窗口 freed 仅窗口对象）；
3. 窗口报告在世界重启后输出，含标记结束后的并发分配 → 相邻周期归属偏移
   （单行 CHECK 后下一行通常反向冲销，demo GC#7 −3.07 / GC#8 +2.94 即实例）；
4. 采样模式的泊松噪声（小样本周期偏差更大，30% 容差内属正常）。


8. Demo 与测试
================

- Demo：`会话内存/demo/main.go` mode=`gcdeadwindow`，`./run.sh gcdeadwindow 10s`，
  flags：`-windowfile`、`-gcinterval`、`-samplerate`。
  MB 量级负载（churn 4KB / live 8KB / burst 16KB）：gen=1 显式 Stop 三阶段
  （P1: 2000 churn + 2000 live；P2: 1500 churn + 1000 burst；P3: 丢 burst +
  1000 churn），gen=2 两秒自动停（每迭代 500×4KB）。
- runtime 测试（gc_test.go TestGcDeadWindow*，均通过）：
  - Basic：基本 alive/alloc/freed 段与 gen 标记；
  - Duration：定时自动 Stop；
  - SessionMutex：窗口/会话双向互斥；
  - GCInterval：3s 窗口 interval=1 ≥3 个报告块 + gcpercent 恢复校验；
  - Sampled：rate 行 + MemProfileRate 设置/恢复校验；
  - NoteReuse：连续裸窗口 note 双 wakeup 回归。


9. 已知限制
=============

- 与 gcdeadtrace 会话模式互斥；同时只能一个窗口。
- 精确模式分配路径开销 ~72×（见 5.8），高分配率服务必须改用采样模式；
  采样模式计数为估计口径（见 5.6 的估算注意事项），小对象可能漏采。
- 窗口 alive 只含窗口期间分配且仍存活的对象，不含窗口前存量；Stop 的最终
  GC 之后遗留对象的释放不再报告（final alive 为 Stop 点真实存活）。
- gctrace 堆值为 MB 截断整数，亚 MB 场景 GC对比 sheet 数值偏粗；sweep 滞后
  服务上 heap 口径列严重失真（用 totalAlloc Δ 主口径）。
- HTTP 端点响应期间占用一个连接（与 CPU profile 相同）；客户端断开则窗口
  提前停止并丢弃报告。
