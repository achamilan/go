gcdeadwindow 设计文档
======================

gcdeadwindow 是 Go runtime 的区间采集调试特性：在一次"窗口"期间跟踪每一次
分配（MemProfileRate 自动置 1），每个 GC 周期输出本周期的 alloc/freed 增量
和窗口累计存活，并与 GC 日志（gctrace）交织写入同一报告文件，可生成 Excel
对比报表。

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


2. API
========

```go
runtime.GcDeadWindowStart(seconds int, path string) bool
runtime.GcDeadWindowStop()
```

- 纯 API 控制，无需 GODEBUG。Start 返回是否成功启动（已有窗口/会话活跃时
  拒绝、打 stderr、返回 false）；调用方可忽略返回值。
- `seconds > 0`：定时 goroutine（`go gcDeadWindowTimer` + notetsleepg）到时
  自动 Stop；`seconds <= 0`：运行直到显式 Stop。
- `path` 非空：报告追加写入该文件（CreateFileW + UTF-8→UTF-16，支持中文
  路径）；空串写 stderr。
- 约束：同时只能有一个窗口；与 gcdeadtrace 会话互斥（双向检查）。
- 副作用：采集期间 MemProfileRate 置 1（Stop 恢复原值）；path 非空时自动
  开启 gctrace（Stop 恢复原值）。

**基线 GC**：Start 在返回前强制一次完整 GC——扫掉窗口前的垃圾作为干净基线，
首个报告块锚定窗口起点，本周期计数器随之清零，后续周期只统计新分配。
**结束 GC**：Stop 强制一次完整 GC，最终报告在 Stop 返回前写出；该 GC 之后
窗口遗留对象的释放不再被报告（final alive 即 Stop 点真实存活，数据验证
alloc == freed + alive 精确成立）。gctrace 原值在这次 GC 之后才恢复。


3. HTTP 接口
==============

`import _ "net/http/pprof"` 后，与 CPU profile 端点同语义——连接保持到
窗口结束，报告作为响应体流回，客户端直接保存到本地：

```
curl -o gcdeadwindow.log http://localhost:6060/debug/gcdeadwindow/start?seconds=30
```

- 仅 start 端点；`seconds` 必须为正整数（缺失/非法 → 400）。
- 响应：200 + `Content-Disposition: attachment; filename="gcdeadwindow.log"`，
  报告含交织的 gctrace 行。
- 并发：HTTP 侧 single-flight（处理中再来请求 → 409）；runtime 拒绝（已有
  窗口/会话活跃）→ 409（依据 GcDeadWindowStart 的 bool 返回值）。
- 服务器侧用临时文件收集（os.CreateTemp），响应发出后删除；最终报告周期
  门控保证 Stop 后的后续 GC 不会再写该文件（不会重建泄漏）。


4. 实现原理
=============

4.1 跟踪机制
-------------

- 复用 `_KindSpecialGcDeadSession` special；窗口对象的 special 携带哨兵
  `sessionID = ^uint64(0)`，与真实会话对象区分。
- `ss.goid` 槽存窗口代数（gen）：每次 Start 递增，旧窗口遗留对象的 free 因
  gen 不匹配被忽略，实现代际隔离。
- memRecord 上 8 个 `gcDeadWin*` 计数器：4 个本周期（每 GC 报告时 Xchg 读清，
  即窗口内部的"stop+start"）+ 4 个窗口累计；alive = cumAllocs − cumFrees。
- 不用会话表/allocBucketRefs，无 per-session/per-goroutine 归属。
- tiny 分配器对象按块计数（memprofile 固有行为）。

4.2 双 GC 触发点与并发
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

4.3 gctrace 同步（tee）
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

4.4 Windows 文件写入
---------------------

`writeDeadTraceToFile`：CreateFileW（FILE_APPEND_DATA + OPEN_ALWAYS，每次
调用独立 open/write/close）+ 手动 UTF-8→UTF-16 转换（surrogate 处理抄
os_windows.go 控制台写法），支持中文路径；`_CreateFileW` 用
`//go:cgo_import_dynamic` 注册并加入 stdFunction 列表。Unix 侧为普通
O_APPEND 写。


5. 输出格式
=============

每个 GC 周期（未被去重时）输出 gctrace 行 + 报告块：

```
gc 7 @0.015s 1%: 0+0.51+0 ms clock, 0+0/1.0/1.0+0 ms cpu, 31->32->26 MB, 32 MB goal, ... (forced)
=== GC #7 gcdeadwindow gen=1 elapsed=0s ===
gcdeadwindow:alive: 2105 objs (31012680 bytes) still alive from 6 sites
  main.winSiteLive (main.go:649) < ...: 2000 objs, 16384000 bytes
gcdeadwindow:alloc: 3345 objs (17623752 bytes) allocated this cycle from 5 sites
gcdeadwindow:freed: 1240 objs (3068040 bytes) freed this cycle from 2 sites
```

- alive：窗口累计存活（cumAllocs − cumFrees），精确字节。
- alloc/freed：本周期增量（Xchg 清零）。
- 同一 GC 可能有两个块（双钩子）；强制 GC 的 post-sweep 块数据更准。


6. Excel 报表
===============

`excel_report_window.go`（无第三方依赖，手写 XLSX）：

```
go run excel_report_window.go output/gcdeadwindow_demo.log   # → gcdeadwindow_report.xlsx
```

四个 sheet：

| Sheet | 内容 |
|-------|------|
| Overview | 各文件汇总（窗口数、报告数、alloc/freed/final alive/peak alive） |
| `<name>` | per-gen 汇总 + 数据验证（alloc == freed + alive）+ Per-GC Summary |
| `<name> - Sites` | 按 (gen, 首帧) 聚合，标记 Fully Dead（alloc>0 且 final alive=0） |
| `<name> - GC对比` | 窗口 alloc/freed (MB) vs gctrace 堆变化，见下 |

**GC对比口径**（gctrace 堆值为全进程 MB 截断整数，窗口为精确字节换算 MB）：

- `GC Alloc MB = heap1[N] − heap2[N−1]`：上周期实际存活 → 本周期标记终止的
  堆增量 ≈ 本周期全进程分配。
- `GC Freed MB = heap1[N] − heap2[N]`：本 GC 清扫出的垃圾 ≈ 本周期全进程释放。
- 解析侧必须**合并同 gcNum 的连续块**（双钩子对同一 GC 各打一块，gctrace
  只有一行），否则第二块用已更新的 prevHeap2 算出虚假 GC Alloc 污染累计列。
- Verdict 按单行差值 ±2MB 判定；累计差值仅作趋势参考（MB 截断带每周期系统
  偏置，长窗口缓慢漂移，不适合做阈值）。

已知系统性差异来源：

1. MB 截断：每值 ±1MB；
2. freed 差值含窗口前旧对象的垃圾（GC freed 全进程，窗口 freed 仅窗口对象）；
3. 窗口报告在世界重启后输出，含标记结束后的并发分配 → 相邻周期归属偏移
   （单行 CHECK 后下一行通常反向冲销，demo GC#7 −3.07 / GC#8 +2.94 即实例）。


7. Demo 与测试
================

- Demo：`会话内存/demo/main.go` mode=`gcdeadwindow`，`./run.sh gcdeadwindow 10s`。
  MB 量级负载（churn 4KB / live 8KB / burst 16KB）：gen=1 显式 Stop 三阶段
  （P1: 2000 churn + 2000 live；P2: 1500 churn + 1000 burst；P3: 丢 burst +
  1000 churn），gen=2 两秒自动停（每迭代 500×4KB）。
- runtime 测试：testprog `GCDeadWindowBasic/Duration/SessionMutex` +
  gc_test.go `TestGcDeadWindow*`（Contains 断言，对额外报告块不敏感）。


8. 已知限制
=============

- 与 gcdeadtrace 会话模式互斥；同时只能一个窗口。
- 采集期间 MemProfileRate=1，高分配率服务有可见性能开销，仅限诊断使用。
- 窗口 alive 只含窗口期间分配且仍存活的对象，不含窗口前存量；Stop 的最终
  GC 之后遗留对象的释放不再报告（final alive 为 Stop 点真实存活）。
- gctrace 堆值为 MB 截断整数，亚 MB 场景 GC对比 sheet 数值偏粗。
- HTTP 端点响应期间占用一个连接（与 CPU profile 相同）；客户端断开则窗口
  提前停止并丢弃报告。
