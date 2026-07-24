gcdeadtrace 实现原理
====================

gcdeadtrace 是 Go runtime 的一个 GODEBUG 调试特性，在每次 GC 周期结束时打印
本周期内会话对象死亡/存活的统计信息。

1. 版本日志
=============

v2:  新增协程级会话追踪，GcDeadSessionStart()/End() 按 goroutine 隔离会话内分配。
v3:  新增会话存活对象追踪，区分"已释放"和"仍存活"的会话内分配。
v4:  新增 per-session 对象标记（span specials），支持多并发会话独立追踪。
v5:  新增类型注解 @type、会话起止位置 [start/end]、存活对象会话归属。[f75933b7]
v6:  移除 bucket 级启发式计数，完全依赖 per-session special 和会话表。
     修复同 bucket 非 session 分配被计入 session 释放的误判问题。[lanyy/996]
v7:  sessionID 改为参数传入，不再自动分配（由业务控制）。新增多 goroutine
     加入同一 session 支持（startSites + joinCount + ended 标志）。
     拆分为两个 GODEBUG 开关：gcdeadsession（会话机制）和 gcdeadtrace（分配追踪）。
     GODEBUG=gcdeadtrace=2 额外输出 session 开始/结束事件日志。
v8:  修复 joinCount/gcDeadSessionCount 计数（per-session 级别而非 per-goroutine）。
     End 时 allgs 遍历清理所有 goroutine 的 session 状态，支持 start-after-end。
     Start 中防御性检查 stale session，自动清理。
v9:  扩展 gcDeadPerSessionSites 至 1024，gcDeadMaxStartSites 至 32。
     gcDeadSessionTable 改为惰性分配指针（persistentalloc），
     不启用 gcdeadtrace 时零内存开销（原 560MB 固定 BSS 数组）。
v10: 移除已废弃的 bucketRefs（死代码），allocBucketRefs 从 1024 扩展至 2048。
     freed 站点追踪改为使用 allocBucketRefs.cumFrees - prevCumFrees 公式，
     移除 per-cycle allocBucketRefs 清零操作，实现跨周期 freed/alive 正确归属。
     新增 prevCumFrees/prevCumFreeBytes 字段用于 freed delta 快照。
     gcDeadRecordFree 简化：移除 bucketRefs 和类型名解析，仅更新 cumFrees。
v11: allocBucketRefs 改为动态扩展（指针 + 长度 + mutex），移除 2048 上限。
     初始大小为 gcDeadPerSessionSites，满时翻倍扩容（persistentalloc），
     无溢出警告。mProf_Malloc 路径无锁探测，仅在扩容时加锁 re-check。
     所有遍历代码改为 for j := uint32(0); j < len; j++ + 数组指针转换。


2. GODEBUG 开关
================

| 开关 | 默认值 | 作用 |
|------|--------|------|
| `gcdeadsession` | `0` | **会话机制**：开启 Start/End API，创建 session 表项，记录边界位置 |
| `gcdeadtrace` | `0` | **分配追踪**：采集每次分配的详情、挂 special、End 时强制 GC。
                         Level 2 额外输出 session 开始/结束事件日志 |

四种组合：

```
GODEBUG=gcdeadsession=1                           → 轻量模式：会话机制开启，无输出
GODEBUG=gcdeadtrace=1                             → 完整模式（向后兼容，等价于两者都开）
GODEBUG=gcdeadsession=1,gcdeadtrace=1             → 完整模式
GODEBUG=gcdeadsession=1,gcdeadtrace=2             → 完整模式 + 输出 session 开始/结束事件日志
GODEBUG=                                           → 关闭（默认）
```

各开关职责：

**`gcdeadsession=1`** — 会话机制：
- `GcDeadSessionStart`/`End` 生效，创建/销毁会话表项
- 设置 goroutine 的 `gcDeadSessionActive`、`gcDeadSessionID`
- 记录 start/end 位置（`startSites`、`endPC`）
- 不触发输出（仅 `gcdeadtrace=1` 时才有输出）

**`gcdeadtrace=1`** — 分配追踪（依赖其启动会话机制）：
- `mProf_Malloc` 中将会话分配计入表计数（`allocs`/`cumAllocs`）
- 挂载 `specialGcDeadSession`，实现释放时精确归属
- `GcDeadSessionStart` 设置 `MemProfileRate=1`
- `GcDeadSessionEnd` 恢复 `MemProfileRate`、触发强制 `GC()`
- 输出中显示 `freed`/`alive` 站点的详细数据

输出对比：

```
// gcdeadtrace=1 完整：
=== GC #1 ===
gcdeadsession by session:
  session #1002: 3 allocs (1304 bytes), 1 freed (256 bytes), 2 alive (1048 bytes)
    [first: main.go:361 (gid=18), end: main.go:372]
gcdeadsession:freed: 1 session objs (256 bytes) freed from 1 sites
  main.(*WorkerActor).patternSession (main.go:361) ...: 1 session objs, 256 session bytes
    [session #1002: 1 objs, 256 bytes @uint8]
gcdeadsession:alive: 2 session objs (1048 bytes) still alive from 1 sites
  main.(*WorkerActor).patternSession (main.go:366) ...: 2 session objs, 1048 session bytes
    [session #1002: 2 objs, 1048 bytes @uint8]

// gcdeadsession=1 轻量：无输出（会话机制工作，但不采集/不输出）
```


3. 核心数据结构
================

3.1 bucket — 全局分配站点
---------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ bucket                                                          │
  │   hash, size, nstk, stack[]     // 分配调用栈 + 大小            │
  │   └→ memRecord {                                               │
  │        active   memRecordCycle   // 已发布的 profiling 快照     │
  │        future   [3]memRecordCycle // 三周期环形缓冲             │
  │                                                                  │
  │        // === gcdeadtrace 全局计数（用于非 session 站点报告）=====│
  │        gcDeadFrees     uintptr   // 本周期死亡对象数             │
  │        gcDeadFreeBytes uintptr   // 本周期死亡字节数             │
  │                                                                  │
  │        // (v6: 原 bucket 级会话追踪字段 gcDeadSessionAllocs 等  │
  │        //  已被移除。session 数据完全从会话表汇总。)            │
  │      }                                                          │
  └─────────────────────────────────────────────────────────────────┘

3.2 specialGcDeadSession — 对象级会话标记
-------------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ specialGcDeadSession (mheap.go, v4 新增)                        │
  │   _    sys.NotInHeap                                            │
  │   special   special    // 嵌入 standard special 头部            │
  │   sessionID uint64     // 所属会话 ID                           │
  │   b         *bucket    // 分配时的 bucket 指针，用于站点归属     │
  │                                                                 │
  │   fixalloc: mheap_.specialGcDeadSessionAlloc                    │
  │   在 GC sweep 时通过 gcDeadRecordFree() 回调                    │
  └─────────────────────────────────────────────────────────────────┘

3.3 gcDeadSessionInfo — 会话表条目
------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ gcDeadSessionInfo (mprof.go, v7 更新)                           │
  │   id         uint64    // 会话 ID (业务传入，唯一)                │
  │   endPC      uintptr   // GcDeadSessionEnd() 调用者 PC (v5)     │
  │   printed    uint32    // 0=未输出, 1=已输出(无end), 2=已输出(有end)│
  │   ended      bool      // End 已调用，不再接受新分配 (v7)        │
  │   joinCount  int32     // 加入该 session 的 goroutine 数 (v7)   │
  │   numStartSites int32  // 已记录的 start 位置数 (v7)            │
  │   startSites [gcDeadMaxStartSites]gcDeadStartSite // 所有 Start  │
  │                                                                 │
  │   allocs       uintptr // 本周期分配数 (per-cycle)               │
  │   allocBytes   uintptr // 本周期分配字节 (per-cycle)            │
  │   frees        uintptr // 累计释放数 (v10 从 per-cycle 改为累计)  │
  │   freeBytes    uintptr // 累计释放字节数                         │
  │   cumAllocs    uintptr // 累计分配数                            │
  │   cumAllocBytes uintptr // 累计分配字节                         │
  │   cumFrees     uintptr // 累计释放数                            │
  │   cumFreeBytes uintptr // 累计释放字节                          │
  │                                                                 │
  │   // 每会话每桶分配+释放计数，用于 freed/alive 站点级 session 归属
  │   allocBucketRefs     *gcDeadSessionBucketRef // v11: 动态扩展指针
  │   allocBucketRefsLen  uint32                   // 当前数组长度
  │   allocBucketRefsLock mutex                    // 扩容保护锁
  │                                                                 │
  │   var gcDeadSessionTable *[gcDeadMaxSessions]gcDeadSessionInfo   │
  │   // (v9: 惰性分配指针，persistentalloc，零内存开销)              │
  │   const gcDeadMaxSessions     = 4096                             │
  │   const gcDeadPerSessionSites = 2048                             │
  │   const gcDeadMaxStartSites   = 32                               │
  └─────────────────────────────────────────────────────────────────┘

3.4 gcDeadStartSite — goroutine 加入位置
------------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ gcDeadStartSite (mprof.go, v7 新增)                             │
  │   goid uint64    // 调用 Start 的 goroutine ID                  │
  │   pc   uintptr   // Start 调用位置                              │
  └─────────────────────────────────────────────────────────────────┘

3.5 gcDeadSessionBucketRef — 站点级累计计数
----------------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ gcDeadSessionBucketRef (mprof.go, 站点归属)                      │
  │   bucket       unsafe.Pointer // *bucket 指针，标识分配站点       │
  │   frees        uintptr        // 累计分配次数                   │
  │   bytes        uintptr        // 累计分配字节数                 │
  │   cumFrees     uintptr        // 累计释放数                    │
  │   cumFreeBytes uintptr        // 累计释放字节数                 │
  │   prevCumFrees     uintptr    // (v10) 上轮 GC 时的 cumFrees 快照│
  │   prevCumFreeBytes uintptr    // (v10) 上轮 GC 时的 cumFreeBytes │
  │   typeName     string         // 类型名称 (v5)                  │
  │                                                                 │
  │   freed 站点计数 = cumFrees - prevCumFrees (本轮释放数, v10)    │
  │   alive 站点计数 = frees - cumFrees (当前存活数)               │
  │   线性探测 O(N) 查找 bucket 指针。                           │
  └─────────────────────────────────────────────────────────────────┘

3.6 gcDeadSessionRef — 输出中 session 归属结构
-------------------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ gcDeadSessionRef (mprof.go, 输出中 session 归属结构)             │
  │   sessionID uint64     // 会话 ID                                │
  │   objs     uintptr     // 对象数 (释放或存活)                     │
  │   bytes    uintptr     // 字节数                                  │
  │   typeName string      // 类型名称 (v5)                          │
  │                                                                  │
  │   在 gcDeadTracePrint Phase 1 中从 allocBucketRefs 交叉引用填入 raw│
  │   条目，输出时渲染为 [session #N: X objs, Y bytes @typeName]    │
  └─────────────────────────────────────────────────────────────────┘

3.7 g 结构体 — goroutine 会话状态
------------------------------------

  ┌─────────────────────────────────────────────────────────────────┐
  │ g struct (runtime2.go)                                          │
  │   gcDeadSessionActive bool   // 当前 goroutine 是否在会话中     │
  │   gcDeadSessionID   uint64  // 当前 goroutine 绑定的会话 ID    │
  │                                                                 │
  │   // 多个 goroutine 可持有相同 gcDeadSessionID (v7)             │
  └─────────────────────────────────────────────────────────────────┘


4. 完整数据流
===============

数据流按时间顺序组织：会话创建 → 分配 → 释放 → GC 汇总输出。

4.1 会话生命周期 — GcDeadSessionStart / GcDeadSessionEnd
-----------------------------------------------------------

  代码位置：mprof.go

  API 签名（v7 变更：sessionID 由业务传入，不再自动分配）：

    func GcDeadSessionStart(id uint64)
    func GcDeadSessionEnd(id uint64)

  Start 语义：
  - 调用 goroutine 加入 sessionId 标识的 session
  - 同一 goroutine 重复调用相同 sessionId → 幂等，无效果
  - 不同 goroutine 调用相同 sessionId → 各自加入同一 session，记录各自 start 位置
  - 如果 session 已结束（End 已调用）→ 当前调用无效

  End 语义：
  - 标记 sessionId 的 session 为"已结束"
  - 只能调用一次（重复调用无效）
  - 已结束的 session 不再接受新分配的归属
  - 已分配但尚未 GC 的对象释放时仍能通过 special 回写 freed 数据

  GcDeadSessionStart(id):
    1. debug.gcdeadsession==0 && debug.gcdeadtrace==0 → 直接返回
    2. 惰性分配会话表（v9）：若 gcDeadSessionTable==nil，加锁后通过
       persistentalloc 分配 [4096]gcDeadSessionInfo。零 BSS 开销。
    3. 获取 getg().m.curg
    4. 若 gp==nil → 返回
    5. 若 gp.gcDeadSessionActive:
       - 检查对应 session 是否已结束（ended 标志）
       - 若已结束 → 自动清理：gp.gcDeadSessionActive = false, id = 0，继续
       - 若未结束 → 返回（幂等）
    6. idx = id % gcDeadMaxSessions, e = &gcDeadSessionTable[idx]
    7. 若 e.ended → 返回（已结束的 session 不加入）
    8. 若 e.id != id → 首次创建，初始化条目：
       - e.id = id, e.endPC = 0, e.ended = false
       - e.joinCount = 1 (v8 修复: 设 1 而不是从 0 累加)
       - e.numStartSites = 1, e.startSites[0] = {goid, pc}
       - 清零所有计数器
    9. 否则 → 后续加入的 goroutine，CAS 添加 startSites[n]
    10. 设置 gp.gcDeadSessionID = id, gp.gcDeadSessionActive = true

    以上为会话机制逻辑（gcdeadsession=1 或 gcdeadtrace=1 时都执行）。

    11. 若 debug.gcdeadtrace > 0:
        - 仅首次初始化时（e.id != id 分支）:
          gcDeadSessionCount.Add(1)
          若首个活跃 session → 保存 MemProfileRate 并设为 1
        - 后续加入的 goroutine 不重复递增这些计数器

  GcDeadSessionEnd(id):
    1. debug.gcdeadsession==0 && debug.gcdeadtrace==0 → 直接返回
    2. 获取 getg().m.curg
    3. 若 gp==nil 或 !gp.gcDeadSessionActive 或 gp.gcDeadSessionID != id → 返回
    4. idx = id % gcDeadMaxSessions, e = &gcDeadSessionTable[idx]
    5. 若 e.id != id 或 e.ended → 返回（幂等保证）
    6. e.endPC = callerPC(), e.ended = true
    7. gp.gcDeadSessionActive = false, gp.gcDeadSessionID = 0
    8. 遍历 allgs，清理所有 gcDeadSessionID == id 的 goroutine 状态
       (v8 修复: 确保其他 goroutine 也能重新 Start 新 session)

    以上为会话机制逻辑（gcdeadsession=1 或 gcdeadtrace=1 时都执行）。

    9. 若 debug.gcdeadtrace > 0:
       - atomic.Xaddint32(&e.joinCount, -1)  (v8: 改为 Add(-1) 而非 Xchg 清零)
       - gcDeadSessionCount.Add(-1)
       - 若无活跃 session → 恢复 MemProfileRate
       - 打印 session 结束位置: "runtime: gcdeadsession: session N ended at file:line"
       - GC() 强制触发 GC，立即输出采集结果


4.2 MemProfileRate 自动管理
-----------------------------

  为什么需要管理采样周期？

    MemProfileRate 是 Go 内存 profiler 的采样率，默认为 512 KB。
    这意味着平均每分配 512 KB 才触发一次 mProf_Malloc。
    如果 gcdeadtrace 依赖 mProf_Malloc 来追踪会话分配，那么默认
    采样率下大部分小对象分配都不会被记录，导致 gcdeadtrace 报告
    严重不完整。

  自动管理机制（仅在 gcdeadtrace>0 时生效）：

    变量                             用途
    ─────────────────────────────────────────────────────────────
    gcDeadSessionCount  atomic.Int32  活跃会话计数（加入的 goroutine 总数）
    gcDeadSavedRate     int           首个会话开始前的 MemProfileRate 值

  GcDeadSessionStart() 中（gcdeadtrace>0 时执行）:
    ┌──────────────────────────────────────────────────────────────┐
    │ // joinCount = 1（首次初始化时设 1，后续 goroutine 不递增）  │
    │ if gcDeadSessionCount.Add(1) == 1 {   // 首个 session 创建   │
    │     gcDeadSavedRate = MemProfileRate    // 保存原始值        │
    │     MemProfileRate = 1                  // 设置为全采集      │
    │ }                                                           │
    └──────────────────────────────────────────────────────────────┘

  GcDeadSessionEnd() 中（gcdeadtrace>0 时执行）:
    ┌──────────────────────────────────────────────────────────────┐
    │ atomic.Xaddint32(&e.joinCount, -1)  // 减 1 而非 Xchg 清零  │
    │ if gcDeadSessionCount.Add(-1) <= 0 {  // 最后一个 session 结束│
    │     MemProfileRate = gcDeadSavedRate   // 恢复原始值         │
    │ }                                                           │
    │ GC()  // 强制触发 GC 立即输出                                │
    └──────────────────────────────────────────────────────────────┘

  gcdeadsession=1 轻量模式的 MemProfileRate 管理：
    - 轻量模式不采集分配数据，不修改 MemProfileRate
    - gcDeadSessionCount 和 gcDeadSavedRate 不被操作
    - 不触发强制 GC()


4.3 分配时采集 — mProf_Malloc
-------------------------------

  当 MemProfileRate=1 时，每个堆分配都经过 mallocgc → profilealloc → mProf_Malloc。

  mProf_Malloc(mp, p, size, typ) 的完整执行路径：

  Step A — 标准 profiling（始终执行）:
    1. callers() 采集调用栈 PC
    2. stkbucket() 查找或创建 bucket（按 size+stack 哈希，全局共享）
    3. 更新 bucket 的 active.allocs++, active.allocBytes+=size

  Step B — gcdeadtrace 会话追踪（仅 debug.gcdeadtrace>0 且在会话中）:

    gp = mp.curg
    if gp != nil && gp.gcDeadSessionActive {
        sid = gp.gcDeadSessionID
        idx = sid % gcDeadMaxSessions
        e = &gcDeadSessionTable[idx]
        if e.id == sid && !e.ended {

    B1 — 更新会话级累计计数（原子操作）:
      - e.allocs++, e.allocBytes+=size  (per-cycle)
      - e.cumAllocs++, e.cumAllocBytes+=size  (累计，永不清零)

    B2 — 更新 allocBucketRefs（站点级分配记录）:

      allocBucketRefs 是动态扩展的 *gcDeadSessionBucketRef 数组
      （v11: 初始 gcDeadPerSessionSites 个，满时翻倍扩容）：

        bp = unsafe.Pointer(b)
        // 基于指针 + len 的线性探测，无锁
        refs = (*[1<<20]T)(unsafe.Pointer(e.allocBucketRefs))
        for j := uint32(0); j < e.allocBucketRefsLen; j++ {
            if refs[j].bucket == bp && refs[j].goid == gp.goid {
                // 完全命中（同 bucket + 同 goroutine）
                atomic.Xadduintptr(&refs[j].frees, 1)
                atomic.Xadduintptr(&refs[j].bytes, size)
                goto mountSpecial
            }
        }
        // 查找空槽插入
        for j := uint32(0); j < e.allocBucketRefsLen; j++ {
            if refs[j].bucket == nil {
                refs[j] = {bucket, goid, frees:1, bytes:size, typeName:tn}
                goto mountSpecial
            }
        }
        // 全满 → 持锁翻倍扩容，re-check 后插入
        lock(&e.allocBucketRefsLock)
        if e.allocBucketRefsLen == oldLen { // double-check
            newLen = oldLen * 2
            newRefs = persistentalloc(newLen * sizeof, ...)
            copy(newRefs[:oldLen], refs[:oldLen])
            newRefs[oldLen] = {bucket, goid, frees:1, ...}
            e.allocBucketRefs = (*T)(unsafe.Pointer(newRefs))
            e.allocBucketRefsLen = newLen
        } // else: 其他协程已扩容 → unlock + retry
        unlock(&e.allocBucketRefsLock)

      frees/bytes 是累计分配计数（v10 后不再清零），
      typeName 来自 toRType(typ).string()。

    B3 — 挂载 special:

      addspecial(p, &specialGcDeadSession{kind, sessionID, b}, false)

      在对象 span 上挂载 _KindSpecialGcDeadSession，携带 sessionID 和 bucket
      指针。未来回收时能精确知道对象的会话归属和分配站点。


4.4 释放时采集 — gcDeadRecordFree
------------------------------------

  4.4.1 与 mProf_Free 的区别

  GC 标记阶段结束后，扫描器会将死亡对象标记为待回收。sweep 阶段会有两个不同的
  入口被调用，它们的作用完全不同：

  mProf_Free（标准 profiling，不参与 gcdeadtrace）:

    func mProf_Free(b *bucket, size uintptr) {
        mp := b.mem()
        mp.active.frees++           // 更新 pprof/ReadMemStats 数据
        mp.active.freeBytes += size
    }

    v6 已移除 bucket 级 gcDeadSession* 启发式计数。mProf_Free 仅服务于
    Go 标准内存 profiling，不参与 gcdeadtrace 数据采集。

  gcDeadRecordFree（通过 special 回调，参与 gcdeadtrace）:

    GC sweep 阶段，sweepone → sweepspan → freeSpecial 遍历已死亡 span 的
    specials 链表。遇到 _KindSpecialGcDeadSession 时调用。

  4.4.2 gcDeadRecordFree 代码路径

    func gcDeadRecordFree(sessionID uint64, size uintptr, b *bucket) {
        idx = sessionID % gcDeadMaxSessions
        e = &gcDeadSessionTable[idx]
        if e.id != sessionID { return }   // 会话表已被覆盖 → 静默丢弃

        // 更新会话级累计释放计数
        atomic.Xadduintptr(&e.frees, 1)
        atomic.Xadduintptr(&e.freeBytes, size)
        atomic.Xadduintptr(&e.cumFrees, 1)
        atomic.Xadduintptr(&e.cumFreeBytes, size)

        // 更新 allocBucketRefs.cumFrees（站点级累计释放）
        if b != nil {
            bp = unsafe.Pointer(b)
            for j := range e.allocBucketRefs {
                if e.allocBucketRefs[j].bucket == bp {
                    atomic.Xadduintptr(&e.allocBucketRefs[j].cumFrees, 1)
                    atomic.Xadduintptr(&e.allocBucketRefs[j].cumFreeBytes, size)
                    return
                }
            }
            // 查找不到 → allocBucketRefs 槽已被覆盖
            // 该站点 freed per-site 数据丢失，但会话级 cumFrees 准确
        }
    }

  v10 简化：
    - bucketRefs 已移除（不再维护独立的 freed 站点追踪表）
    - 类型名解析 (toRType(typ).string()) 已移除（typeName 来自分配时记录）
    - 释放时仅更新 allocBucketRefs.cumFrees


4.5 GC 结束时汇总 — gcDeadTracePrint
---------------------------------------

  在 GC 标记结束的 STW 阶段末尾调用（gcMarkDone → gcDeadTracePrint，
  仅在 gcdeadtrace>0 时）。六阶段流水线：

  Phase 0 — 原始 bucket 数据收集
    遍历全局 mbuckets 链表，对 gcDeadFrees>0 的 bucket 创建 raw 条目。
    (v6: 不再读取 bucket 级 session 计数器，session 数据完全由会话表提供。)

  Phase 1 — 交叉引用 allocBucketRefs（核心计算阶段）

    遍历 gcDeadSessionTable，对每个活跃 session 的 allocBucketRefs[0..2047]：

      for j := range e.allocBucketRefs {
          br = &e.allocBucketRefs[j]
          if br.bucket == nil { continue }

          // 提取 bucket 的调用栈 PC，在 raw 条目中按 PC 元组匹配

          // === freed 站点归属 (v10) ===
          freedObjs = br.cumFrees - br.prevCumFrees     // 本轮释放数
          freedBytes = br.cumFreeBytes - br.prevCumFreeBytes
          if freedObjs > 0 {
              raw.sessionRefs[idx] = {sessionID, freedObjs, freedBytes, typeName}
          }

          // === alive 站点归属 (v6) ===
          aliveObjs = br.frees - br.cumFrees             // 当前存活数
          aliveBytes = br.bytes - br.cumFreeBytes
          if aliveObjs > 0 {
              raw.aliveSessionRefs[idx] = {sessionID, aliveObjs, aliveBytes, typeName}
          }
      }

    设计要点：
      - 每个 raw 条目最多挂载 16 个 session 归属 (gcDeadSessionRefSlots)
      - 匹配基于原始 PC 元组（Phase 3 符号化之前），避免字符串比较
      - 同一桶跨会话释放时各自独立记录
      - 匹配失败且 rawCount 未超限 → 自动创建新的 raw 条目 (v6)

    核心公式：

    | 指标 | 公式 | 含义 |
    |------|------|------|
    | 本轮 freed | cumFrees - prevCumFrees | 自上次 GC 以来该站点释放数 |
    | 当前 alive | frees - cumFrees | 该站点分配的仍存活对象数 |

  Phase 2 — 输出 per-session 分解（v4 新增，v6/v7 更新）
    遍历 gcDeadSessionTable，对每个活跃条目 (id != 0)：
      - 输出 allocs, allocBytes, frees, freeBytes, alive, aliveBytes
      - v6: 累加 totalSessionFrees/Alive 用于汇总行
      - 清零 per-cycle 计数器 (allocs/allocBytes)，保留累计计数器
      - 根据 endPC 设 printed=1 或 2

    输出格式（v7）：
      gcdeadsession by session:
        session #1002: 3 allocs (1304 bytes), 1 freed (256 bytes), 2 alive (1048 bytes)
          [first: main.go:361 (gid=18), end: main.go:372]

      first: 首个 Start goroutine | join: 后续加入 goroutine | end: End 调用位置

  Phase 3 — 符号化
    按分配站点维度合并并符号化（PC → 函数名/文件/行号），
    合并时合并 sessionRefs/aliveSessionRefs（去重 sessionID）。

  Phase 4 — 排序
    按 bytes 降序排列站点。

  Phase 5 — 格式化输出 + 更新 prevCumFrees

    gcdeadsession:freed: N objs (M bytes) freed from K sites
      func (file:line): N objs, M bytes [session #N: X objs, Y bytes @type]
    gcdeadsession:alive: N objs (M bytes) still alive from K sites
      func (file:line): N objs, M bytes [session #N: X objs, Y bytes @type]

    v10 新增：输出后更新 prevCumFrees
      遍历所有活跃 session 条目的 allocBucketRefs：
      prevCumFrees = cumFrees, prevCumFreeBytes = cumFreeBytes
      为下轮 GC 的 freed delta 设置基准。

  Phase 6 — 写入输出
    write(2, buf, n) 到 stderr，若 gcdeadtracefile 配置 → 追加写入文件。


4.6 站点级 session 归属原理
-----------------------------

  gcdeadsession:freed 和 gcdeadsession:alive 的站点行上附加
  [session #N: X objs, Y bytes @typeName] 的实现机制：

  1. 分配时：mProf_Malloc 记录 allocBucketRefs + 挂载 special
     - allocBucketRefs[].frees++ 记录分配
     - allocBucketRefs[].typeName = toRType(typ).string() 捕获类型
     - special{b: bucket} 保存 bucket 指针

  2. 释放时：gcDeadRecordFree 更新 cumFrees
     - 查找 allocBucketRefs，更新 cumFrees/cumFreeBytes

  3. GC 结束时：gcDeadTracePrint Phase 1 交叉引用
     - freed = cumFrees - prevCumFrees → sessionRefs
     - alive = frees - cumFrees → aliveSessionRefs

  4. 输出时：Phase 5 遍历 site 的 sessionRefs/aliveSessionRefs

  数据流图（v10）：

    mProf_Malloc (分配)
      → allocBucketRefs[].frees++, .typeName
      → special{b: bucket} 挂载
              ↓
    gcDeadRecordFree (释放)
      → allocBucketRefs[].cumFrees++
              ↓
    gcDeadTracePrint Phase 1
      → freed = cumFrees - prevCumFrees → sessionRefs
      → alive = frees - cumFrees → aliveSessionRefs
              ↓
    gcDeadTracePrint Phase 5
      → 输出 [session #N: X objs, Y bytes @type]
      → prevCumFrees = cumFrees  (准备下一轮)


5. 原理深入
=============

5.1 多会话隔离原理
--------------------

  场景：两个并发 goroutine 从相同分配站点 (make([]byte, 256)) 分配内存。
  传统 bucket 级追踪无法区分释放对象所属的会话。

  使用 per-session special (v4) 的隔离流程：

  分配时：
    Goroutine A (gid=7, session=1001):
      → mProf_Malloc: 找到 bucket X (size=256, stack=...)
      → 挂载 special{sessionID: 1001}, 写 allocBucketRefs

    Goroutine B (gid=8, session=1002):
      → mProf_Malloc: 找到 SAME bucket X
      → 挂载 special{sessionID: 1002}, 写 allocBucketRefs（不同数组）

  释放时：
    Session 1001 对象 → gcDeadRecordFree(sessionID=1001) → 只影响 session 1001
    Session 1002 对象 → gcDeadRecordFree(sessionID=1002) → 只影响 session 1002

  GC 结束时输出：
    session #1001: 3 freed (768 bytes), 1 alive (1024 bytes)
    session #1002: 2 freed (512 bytes), 1 alive (512 bytes)

  session #1001 的三次释放仅包含其自身的释放，不混合 session #1002。

  精度保证：
    - v4 (per-session special): 每个对象带有 sessionID → 精确
    - v6: 移除 bucket 级启发式 → 彻底消除同 bucket 非 session 分配的误计
    - v7: sessionID 由业务传入 → 支持多 goroutine 共享同一 session


5.2 会话存活对象追踪
----------------------

  通过累计计数追踪存活：

    分配: cumAllocs++ (仅更新)
    释放: cumFrees++  (同时更新 per-cycle 和 cumulative)
    GC:   alive = cumAllocs - cumFrees

  示例：

    ┌────────────────┬────────┬──────────┬──────────────────────────┐
    │ 会话           │ 大小   │ 状态     │ 每个 GC 周期的报告       │
    ├────────────────┼────────┼──────────┼──────────────────────────┤
    │ Session 1001   │ 256B   │ 已释放   │ gcdeadsession:freed      │
    │ Session 1001   │ 1024B  │ 仍存活   │ session 1001 alive = 1   │
    │ Session 1002   │ 256B   │ 已释放   │ gcdeadsession:freed      │
    │ Session 1002   │ 512B   │ 仍存活   │ session 1002 alive = 1   │
    └────────────────┴────────┴──────────┴──────────────────────────┘

  v5 新增: 类型注解 (@typeName) 来源于 toRType(typ).string()
  v10: allocBucketRefs.frees 改为累计值，alive = 累计分配 - 累计释放


5.3 per-session 表生命周期
----------------------------

  表条目 (`gcDeadSessionTable[sessionID % 4096]`)：

  创建：
    GcDeadSessionStart(id) 被调用
    - 若 e.id != id → 初始化条目
    - 若 e.id == id → 只新增 startSites

  活跃期：
    分配: 更新 allocs/cumAllocs (仅 gcdeadtrace>0)
    释放: 更新 frees/cumFrees + allocBucketRefs.cumFrees
    GC:   读取并清零 per-cycle 字段，更新 prevCumFrees

  结束：
    GcDeadSessionEnd 设置 endPC 和 ended=true
    - ended 后不接受新分配归属
    - 已挂载 special 的对象释放时仍能回写 freed 数据

  重用：
    当 hash 冲突时，新 session 覆盖旧条目
    gcDeadRecordFree 检查 e.id == sessionID → 旧 specials 回调安全忽略

  常量:
    最大并发 sessions:    4096 (gcDeadMaxSessions)
    Per-session 站点上限: 动态扩展（初始 2048，满时翻倍）
    最大 start 位置数:    32   (gcDeadMaxStartSites)
    Session 归属 slot 数: 16   (gcDeadSessionRefSlots)


6. 历史修复记录 — Bucket 级启发式修复（v6）
=============================================

6.1 问题背景
--------------

  在测试中发现：gcdeadsession:freed 总数（1,052,552）
  远大于 per-session freed 之和（~204,531），差值约 848K（80%）。

6.2 根因分析
--------------

  mProf_Free 使用 bucket 级永久标记来判断对象是否为 session 分配：

    func mProf_Free(b *bucket, size uintptr) {
        if mp.gcDeadSessionAllocBytes > 0 {    // ← 一旦>0，永不清零
            atomic.Xadduintptr(&mp.gcDeadSessionFrees, 1)
        }
    }

  gcDeadSessionAllocBytes 在 mProf_Malloc 中设置，只增不减。一旦某个 bucket
  有过 session 分配，该 bucket 的所有后续释放（不论是否由 session goroutine
  产生）都被计入 gcdeadsession:freed 总数。

  对比：per-session 分解靠的是 special — 只有挂了 _KindSpecialGcDeadSession
  的对象才会触发 gcDeadRecordFree，这是精确的。两者差异的来源：

    Bucket "make([]byte,256)":
      分配:
        - 协程 A (session #1001): 挂 special
        - 协程 B (非session):     不挂 special (但 gcDeadSessionAllocBytes 已>0)
      释放:
        - 协程 A 的对象: mProf_Free + gcDeadRecordFree → 两方都计
        - 协程 B 的对象: mProf_Free 计, 无 gcDeadRecordFree → 仅 bucket 级计数
                                                                  ↑ gap

6.3 修复方案
--------------

  采用"方案 A"：完全移除 bucket 级启发式计数，所有 session 数据的 freed 和
  alive 计数完全从 session 表（gcDeadSessionTable）汇总。

  主要改动：
  1. memRecord 删除 gcDeadSessionAllocs/gcDeadSessionFrees 等字段
  2. mProf_Free 删除 if gcDeadSessionAllocBytes > 0 代码块
  3. mProf_Malloc 删除 bucket 级计数代码
  4. gcDeadTracePrint Phase 1 不再读取 bucket 级 session 计数器
  5. gcDeadRecordFree 新增 allocBucketRefs.cumFrees 更新
  6. gcDeadSessionBucketRef 新增 cumFrees/cumFreeBytes 字段

6.4 测试验证
--------------

  TestGcDeadTraceBucketOvercount:
    - 同一协程：session + 非 session 分配同 bucket
    - 预期：freed=1（仅 session 分配），非 session 分配不被计入
    - 结果：PASS

  TestGcDeadTraceBucketOvercountConcurrent:
    - 两并发 session 各分配 + 非 session 分配同 bucket
    - 预期：freed=2，非 session 分配不被计数
    - 结果：PASS


7. 已知限制
=============

7.1 会话表容量限制 (gcDeadMaxSessions = 4096)
  会话表首次调用 GcDeadSessionStart 时通过 persistentalloc 分配。
  不启用时零内存开销（v9 惰性分配指针）。
  当并发 session 数超过 4096 时，新 session 覆盖旧 session 的表条目，
  旧 session 后续释放数据丢失：

    - per-session freed 计数少 1
    - allocBucketRefs 站点归属丢失
    - alive 计数偏高（cumFrees 偏小）

  安全性：e.id != sessionID 严格检查保证不误计到其他 session。

7.2 Per-Session 分配站点跟踪 (gcDeadPerSessionSites = 2048)
  v10 从 1024 扩展。v11 改为动态扩展：初始 2048，满时翻倍，
  移除 LRU 替换和溢出警告。参见 7.12。

7.3 全局站点行归属限制 (gcDeadSessionRefSlots = 16)
  站点行 per-session 归属标注最多支持 16 个 session。v6 从 4 增大至 16。

7.4 全局总分配站点数限制 (gcDeadTraceMaxSites = 4096)
  每次 GC 输出最多聚合 4096 个不同分配站点。超出部分被丢弃。

7.5 调用栈帧深度限制 (gcDeadTraceMaxFrames = 3)
  分配站点区分最多截取 3 帧（跳过 runtime 帧）。

7.6 Per-Session alive 站点计数精度
  v10 将 allocBucketRefs.frees 从 per-cycle 改为累计值，
  cumFrees 也是累计值，alive = 累计分配 - 累计释放。
  但若 allocBucketRefs 槽位被覆盖，旧站点数据丢失，alive 轻微偏高。

7.7 类型名称解析精度
  toRType(typ).string() 对匿名结构体、泛型等可能返回简化描述。

7.8 运行时启用限制
  GODEBUG 需进程启动时设置。启用后 MemProfileRate=1，所有 malloc
  都走 profiling 路径，影响性能。仅建议用于诊断场景。

7.9 gcdeadtracefile 路径限制
  Windows 下使用 CreateFileA (ANSI)，不支持非 ASCII 字符。

7.10 多 goroutine startSites 上限 (gcDeadMaxStartSites = 32)
  超过 32 个 goroutine 的 Start 位置不记录（输出不显示）。

7.11 End 后 allgs 遍历的锁竞争
  GcDeadSessionEnd 中遍历 allgs 需持有 allglock，高频 End 可产生竞争。

7.12 allocBucketRefs 动态扩展
  v11 移除了固定大小（2048）限制，改用翻倍扩容策略：
  - 初始 gcDeadPerSessionSites（2048）个槽位
  - 满时翻倍（4096, 8192, ...），persistentalloc 零拷贝成本
  - mProf_Malloc 路径无锁线性探测，仅在扩容瞬态加锁
  - 无须溢出警告


8. 关键文件
=============

  src/runtime/runtime2.go           — g.gcDeadSessionActive, g.gcDeadSessionID
  src/runtime/mheap.go              — specialGcDeadSession struct + fixalloc
  src/runtime/mprof.go              — gcDeadSessionTable + 所有追踪逻辑
  src/runtime/mprof.go              — mProf_Malloc (gcdeadtrace>0 时的 session 归属)
  src/runtime/mprof.go              — gcDeadRecordFree (special 回调)
  src/runtime/mprof.go              — gcDeadTracePrint (6 阶段流水线)
  src/runtime/mprof.go              — GcDeadSessionStart/GcDeadSessionEnd
  src/runtime/malloc.go             — mallocgc 传递 typ 到 profilealloc
  src/runtime/mgc.go                — GC() 结束时调用 gcDeadTracePrint()
  src/runtime/runtime1.go           — GODEBUG 注册
  src/runtime/create_file_unix.go   — Unix 文件写入
  src/runtime/create_file_windows.go — Windows 文件写入
  src/runtime/create_file_nounix.go  — 其他平台 no-op
  src/runtime/testdata/testprog/gc.go — 测试程序
  src/runtime/gc_test.go            — 测试函数


9. 完整流程全景：一次分配的一生
================================

本章以一条分配语句 `obj := make([]byte, 256)` 为例，追踪它从诞生到在 gcdeadtrace
输出中出现的完整路径。

场景假设：用户代码在两个 GcDeadSessionStart/End 之间分配了一个对象：

```go
runtime.GcDeadSessionStart(42)          // 步骤 1
obj := make([]byte, 256)                // 步骤 2
// ... 使用 obj ...
runtime.GcDeadSessionEnd(42)            // 步骤 3 → 触发 GC，步骤 4-7 在 GC 内部完成
```

整个流程分为 7 个阶段，按执行顺序逐一描述。

**—— 以下是快速讲解版，约 3 分钟读完 ——**

gcdeadtrace 的目标是追踪每个会话内分配对象的最终结局：对象死亡时属于哪个会话、
死在哪个分配站点。实现依赖分配时和释放时两个采集点。

**分配时（mProf_Malloc）采集三条信息：**

1. 对象所属会话（sessionID）
2. 对象的分配站点（调用栈 + 分配大小）
3. 对象的类型名称

sessionID 用于区分不同会话的数据；调用栈 + 大小定义站点，用于在输出中按站点归类；
类型名称用于站点行上的 `@[]uint8` 注解。

分配站点由调用栈和分配大小共同决定，两者缺一不可：

```go
func doAlloc(size uintptr) {
    obj := make([]byte, size)   // 调用栈: doAlloc
}

doAlloc(256)  // 站点 A: 调用栈=doAlloc + 大小=256
doAlloc(512)  // 站点 B: 调用栈=doAlloc + 大小=512（大小不同）

func doAlloc2() {
    obj := make([]byte, 256)    // 站点 C: 调用栈=doAlloc2 + 大小=256（调用栈不同）
}
```

相同站点的所有分配在输出中汇总为一条站点行。

**释放时（gcDeadRecordFree）采集一条信息：**

4. 对象所属会话的释放事件

**最终输出包含三个部分：**

```
=== GC #1 ===
gcdeadsession by session:
  session #42: 1 allocs (256 bytes), 1 freed (256 bytes), 0 alive (0 bytes)
    [first: main.go:15 (gid=1), end: main.go:19]

gcdeadsession:freed: 1 session objs (256 bytes) freed from 1 sites
  main.main (main.go:15): 1 session objs, 256 session bytes
    [session #42: 1 objs, 256 bytes @[]uint8]

gcdeadsession:alive: 0 session objs (0 bytes) still alive from 0 sites
```

- by session：每个会话的总览（分配数、释放数、存活数）
- freed：按站点列出本轮 GC 回收的会话对象及其会话归属
- alive：按站点列出当前仍存活的会话对象及其会话归属

**数据流简图：**

```
分配时 (mProf_Malloc)
  → 采集 sessionID、调用栈、类型
  → 更新会话表计数
  → 挂 special 标签到对象
       ↓
释放时 (gcDeadRecordFree, 通过 special 回调)
  → 采集释放事件（所属会话 + 站点）
  → 更新会话表释放计数
       ↓
汇总输出 (gcDeadTracePrint)
  → freed = 本轮释放 − 上轮快照
  → alive = 累计分配 − 累计释放
  → 输出 per-session 总览、freed 站点明细、alive 站点明细
```

整体流程可概括为：**分配时记"谁从哪来"，释放时记"谁走了"，GC 结束时算"还剩谁、死在哪"**。

**—— 快速讲解结束，以下是完整文字描述 ——**


9.1 GcDeadSessionStart — 会话创建
----------------------------------

GcDeadSessionStart(42) 被调用时，首先检查两个 GODEBUG 开关。如果都没开，直接返回，
零开销。

当 gcdeadsession=1 或 gcdeadtrace=1 时，进入会话机制逻辑。如果是首次调用，会惰性
分配会话表：gcDeadSessionTable 是一个 [4096]gcDeadSessionInfo 的指针，通过
persistentalloc 分配，不启用时完全不占内存。调用 getg().m.curg 获取当前 goroutine，
如果当前不在 goroutine 上下文（如系统线程），则跳过。

如果当前 goroutine 已经在某个活跃会话中（gp.gcDeadSessionActive 为 true），检查
之前绑定的会话是否已被其他 goroutine 结束（通过 gp.gcDeadSessionID 查找对应表条目
的 ended 标志）。如果已结束，自动清理 goroutine 的会话状态并继续。如果未结束，说明
重复调用 Start，幂等返回。

接下来计算表索引 `idx = 42 % 4096 = 42`，获取表条目 `e = &gcDeadSessionTable[42]`。
此处有一段复用检测逻辑：如果 e.originalID == 42 且 e.ended 为 true，说明会话 42
曾经用过并已结束，现在是重复使用。此时创建一个新的 generation：从 e.generation
读取当前最高代际，用公式 `internalID = 42 * 4096 + (generation + 1)` 计算内部 ID，
然后在新哈希槽上初始化条目。这保证了不同代际的会话数据不冲突，输出中显示为
`session #42#1`。

如果没有检测到复用，检查 e.ended。如果已结束，返回（不加入已结束的会话）。

然后检查 e.originalID 是否等于 42。如果不相等，是首次初始化：设置所有计数器为零，
记录起始位置（startSites[0]），最终发布时设置 `e.id = 42`、`e.originalID = 42`、
`e.generation = 0`。如果相等，说明是另一个 goroutine 加入同一会话，通过 CAS 在
startSites 数组中添加自己的位置。

最后设置 goroutine 状态：`gp.gcDeadSessionID = 42`、`gp.gcDeadSessionActive = true`。
如果 gcdeadtrace>0 且是首次创建会话，还会递增 gcDeadSessionCount，如果是第一个
活跃会话则保存 MemProfileRate 并设为 1（确保每个分配都被采集），并输出
"session 42#0 started at file:line"。


9.2 mProf_Malloc — 分配时的标准 profiling
-------------------------------------------

当 `make([]byte, 256)` 执行时，mallocgc 内部调用 profilealloc，最终到达
mProf_Malloc。这是每个堆分配的必经之路（MemProfileRate=1 时）。

首先执行标准的内存 profiling：通过 callers() 采集当前调用栈 PC，调用 stkbucket()
按 size + 调用栈哈希查找或创建全局 bucket。bucket 是全局共享的，相同 size 和调用栈
的分配共享同一个 bucket。然后更新 bucket 的 `mpc.allocs++` 和
`mpc.alloc_bytes += size`，服务于 pprof 和 ReadMemStats。

这个标准 profiling 与 gcdeadtrace 无关，但 gcdeadtrace 复用了它的 bucket 指针
来做站点级归属。


9.3 mProf_Malloc — gcdeadtrace 会话追踪（B1：会话级计数）
----------------------------------------------------------

标准 profiling 完成后，进入 gcdeadtrace 代码块（由 debug.gcdeadtrace > 0 控制）。
检查 `mp.curg` 是否非空且 `gp.gcDeadSessionActive` 为 true。如果不满足，跳过
整个 gcdeadtrace 追踪。

满足条件后，用 `gp.gcDeadSessionID`（内部 ID，可能是 42 或 42*4096+1 等）计算
表索引并查找条目。校验 `e.id == sid && !e.ended` 通过后，执行四个原子操作更新
会话级计数：

- `atomic.Xadduintptr(&e.allocs, 1)` — 本轮分配数
- `atomic.Xadduintptr(&e.allocBytes, 256)` — 本轮分配字节
- `atomic.Xadduintptr(&e.cumAllocs, 1)` — 累计分配数（永不清零）
- `atomic.Xadduintptr(&e.cumAllocBytes, 256)` — 累计分配字节

同时更新 per-goroutine 统计：在 e.goroutineStats 数组中线性查找匹配的 goid，
更新 `allocs++` 和 `allocBytes += size`。这样输出时能展示每个 goroutine 在
会话内的分配和释放明细。

最后解析类型名称：如果 typ 非空，调用 `toRType(typ).string()` 获取类型字符串
（如 "[]uint8"），存储在 allocBucketRefs 中用于输出时的 `@typeName` 注解。


9.4 mProf_Malloc — allocBucketRefs 站点级记录（B2）
-----------------------------------------------------

会话级计数之后，进入站点级记录。这里的核心数据结构是 allocBucketRefs——
一个动态扩展的 `gcDeadSessionBucketRef` 数组，为当前会话关联的每个分配站点
（按 bucket 指针 + goid 区分）维护独立的计数。

第一次访问时，allocBucketRefs 为 nil，代码会通过 persistentalloc 懒惰地分配
初始容量（gcDeadPerSessionSites = 2048 个槽位）。然后跳回搜索入口。

后续每次分配走线性探测流程，全部无锁（仅在扩容瞬态加锁）：

第一轮遍历：按 `bucket 指针 + goid` 双条件匹配。如果找到已存在的记录（同一分配
站点、同一 goroutine），对其 `frees++` 和 `bytes += size` 做原子递增，然后跳到
后续的 special 挂载步骤。这种命中场景最常见：同一个 goroutine 反复从同一代码位置
分配对象。

第二轮遍历：如果第一轮未命中，查找空槽（bucket 为 nil 的槽位）。找到后非原子地
写入 `{bucket, goid, frees:1, bytes:size, typeName}`，然后跳到 special 挂载。

两轮都未找到且没有空槽：说明数组满了。此时对 gcDeadSessionInfo 的
allocBucketRefsLock 加锁，double-check 确认数组未被其他 goroutine 扩容后，
通过 persistentalloc 分配两倍大小的新数组，拷贝旧数据并在末尾插入当前记录，
然后更新指针和长度后解锁。如果 double-check 发现已被其他 goroutine 扩容，直接
解锁并跳回搜索入口重试。

allocBucketRefs 的单条记录包含以下字段：

- bucket (unsafe.Pointer)：指向 bucket 的指针，用于站点标识
- goid (uint64)：分配时的 goroutine ID
- frees (uintptr)：累计分配次数（自该会话首次分配以来）
- bytes (uintptr)：累计分配字节
- cumFrees (uintptr)：累计释放次数（由 gcDeadRecordFree 更新）
- cumFreeBytes (uintptr)：累计释放字节
- prevCumFrees / prevCumFreeBytes (uintptr)：上轮 GC 时的 cumFrees 快照，
  用于计算本轮 freed 增量
- typeName (string)：类型名称，如 "[]uint8"


9.5 mProf_Malloc — 挂载 specialGcDeadSession（B3）
-----------------------------------------------------

站点级记录完成后，进入 special 挂载阶段。这个 special 是 gcdeadtrace 实现
**精确释放归属**的关键：对象在释放时必须通过 special 知道它属于哪个会话。

从 mheap_.specialGcDeadSessionAlloc（fixalloc 分配器）分配一个
specialGcDeadSession 结构体，填入以下信息：

- `ss.special.kind = _KindSpecialGcDeadSession` — 标记为 gcdeadtrace 专用
- `ss.sessionID = gp.gcDeadSessionID` — 内部会话 ID
- `ss.goid = gp.goid` — 当前 goroutine ID（用于输出中 per-goroutine 归属）
- `ss.b = b` — bucket 指针（用于释放时查找 allocBucketRefs）
- `ss.typ = typ` — 类型元数据（用于释放时获取 typeName）

然后调用 `addspecial(p, &ss.special, false)` 将这个 special 挂载到对象 p
所在 span 的 specials 链表上。每个 span 的 specials 链表是一个单向链表，
按 kind 分类。如果该对象已经有同类型的 special，addspecial 返回 false，
此时释放刚分配的 special 结构体。

至此，堆上每个会话对象都与一个 specialGcDeadSession 关联。后续 GC sweep 时，
sweep 代码会遍历已死亡 span 的 specials 链表，遇到 _KindSpecialGcDeadSession
就回调 gcDeadRecordFree，从而精确地知道这个释放的对象属于哪个会话、从哪个
bucket 分配。这个机制是跨会话隔离的基石：即使两个不同会话从相同的分配站点
分配内存，它们的 special 携带不同的 sessionID，释放时各自更新对应会话的计数器。

与 mProf_Free 的区别值得注意：mProf_Free 仅在 bucket 级别更新 `frees++`，
不涉及任何会话归属。gcdeadtrace 的释放追踪完全通过 special 回调完成，两者是
独立的路径。


9.6 GcDeadSessionEnd — 会话结束 + 触发 GC
--------------------------------------------

GcDeadSessionEnd(42) 被调用时，首先核实当前 goroutine 确实在某个活跃会话中。
如果 gp.gcDeadSessionActive 为 false，直接返回。

然后检查 gcDeadSessionTable，用 gp.gcDeadSessionID（内部 ID）查找条目，验证
`e.originalID == id` 确保用户传入的原始 ID 匹配。通过后：

- 清理当前 goroutine 状态：`gp.gcDeadSessionActive = false`，
  `gp.gcDeadSessionID = 0`
- 遍历 allgs（持有 allglock），清理所有 `gcDeadSessionID` 等于内部 ID 的 goroutine
  状态。这确保即使有其他 goroutine 也加入了此会话，它们都能被释放以启动新会话。
- 如果 gcdeadtrace > 0：设置 `e.endPC` 为调用者 PC、`e.ended = true`，
  递减 `gcDeadSessionCount`（到 0 则恢复 MemProfileRate 为原始值），
  输出 "session 42#0 ended at file:line"。
- 最后调用 `GC()` 强制触发一次垃圾回收，立即产生输出。


9.7 GC 标记与 Sweep — 对象回收与 gcDeadRecordFree
----------------------------------------------------

GC() 启动后，标记阶段扫描器发现 obj 已无引用（用户代码将 obj 置 nil 或超出
作用域），将其标记为死亡。span 的 specials 链表随之进入待处理状态。

sweep 阶段按 page 粒度回收内存。sweepone 函数每次找一个未清扫的 span，调用
sweepspan 处理。sweepspan 内遍历 span 上的所有已死亡对象，对每个对象调用
freeSpecial。freeSpecial 遍历对象的 specials 链表，按 kind 分发：

遇到 `_KindSpecialGcDeadSession` 时，从 special 中提取 sessionID、bucket
指针 b、goid 和 typ，调用 gcDeadRecordFree。gcDeadRecordFree 的完整流程：

首先计算索引 `idx = sessionID % 4096`，获取会话表条目 e。校验 `e.id == sessionID`
确保表条目未被其他会话覆盖。如果不匹配，静默丢弃（该会话已被重用，旧数据丢失）。

匹配成功后执行四部分更新，全部使用原子操作：

1. 会话级 freed 计数：`e.frees++`、`e.freeBytes += size`（累积释放量，
   用于输出中的 per-session freed 统计）。同时更新 `e.cumFrees++`、
   `e.cumFreeBytes += size`（累计值永不清零）。

2. per-goroutine 释放统计：在 e.goroutineStats 数组中按 goid 匹配，
   更新 `frees++` 和 `freeBytes += size`。

3. 站点级累计释放：将 special 中存储的 bucket 指针转为 unsafe.Pointer，
   在 e.allocBucketRefs 数组中线性查找匹配的 bucket 指针。找到后更新
   `cumFrees++` 和 `cumFreeBytes += size`。如果找不到（槽位已被覆盖），
   该站点 freed 数据丢失，但会话级计数仍准确。

4. 类型名解析：`toRType(typ).string()` 获取类型字符串用于输出注解。

一个需注意的细节：allocBucketRefs 查找时仅按 bucket 指针匹配，不做 goid 匹配。
这是因为 gcDeadRecordFree 使用 special 中存储的 goid（分配时的 goroutine），
不是当前执行释放的 goroutine。如果同一个 bucket 被多个 goroutine 分配，
释放时按 bucket 匹配会累加到一个记录上。这是设计上可接受的近似。


9.8 gcDeadTracePrint — GC 结束时的六阶段输出
-----------------------------------------------

所有 sweep 完成后，在 GC 标记结束的 STW 阶段末尾，gcMarkDone 调用
gcDeadTracePrint（仅在 gcdeadtrace > 0 时生效）。这是一个六阶段流水线，
全部在 STW 中执行以保证数据一致性。

**Phase 0 — 原始 bucket 数据收集。** 遍历全局 mbuckets 链表，对每个
gcDeadFrees > 0 的 bucket，将其调用栈 PC、释放次数和字节数写入 raw 数组。
此时 raw 数组只含非 session 的全局 freed 站点，session 数据在下一阶段加入。

**Phase 1 — 交叉引用 allocBucketRefs（核心计算阶段）。** 遍历整个
gcDeadSessionTable，对每个活跃会话遍历其 allocBucketRefs。用两个关键公式
计算 freed 和 alive 计数：

- `本轮 freed = cumFrees - prevCumFrees`：自上次 GC 以来该站点释放了多少对象
- `当前 alive = frees - cumFrees`：该站点分配的仍存活的对象数

对于 freed > 0 的对象，构造 sessionRefs 条目，包含 sessionID、originalID、
generation、objs、bytes 和 typeName，挂载到匹配的 raw 条目上（按 PC 元组匹配，
不做字符串比较，纯指针比较）。如果找不到匹配 raw 条目且未超限，创建新 raw 条目。
alive 的归属同理，写入 aliveSessionRefs。

此时 session 数据的 originalID 和 generation 从会话表传播到了 raw 条目中。
后续输出时，如果 generation > 0，会显示为 `session #42#1`，否则显示为
`session #42`（兼容旧格式）。

**Phase 2 — per-session 分解输出。** 再次遍历 gcDeadSessionTable，对每个
有数据的会话输出按 session 汇总行："session #42: N allocs (M bytes),
K freed (L bytes), P alive (Q bytes)"，附带 start 位置、join 位置和 end 位置。
清零 per-cycle 计数器（frees/freeBytes），保留累计计数器（cumAllocs/cumFrees）。

**Phase 3 — 符号化 + 合并。** 将 raw 条目中的 PC 解析为函数名、文件名和行号。
按符号化后的 key（函数+文件+行号三元组）合并站点。合并时，sessionRefs 按
sessionID 去重累加（同一 session 在同一站点的 freed 计数合并）。

**Phase 4 — 排序。** 所有合并后的站点按 bytes 降序排列。

**Phase 5 — 格式化输出 + 更新 prevCumFrees。** 组织三部分输出：

1. per-session 分解（Phase 2 已完成追加到缓冲区）
2. freed 报告："gcdeadsession:freed: N objs (M bytes) freed from K sites"，
   每个站点显示调用栈和 [session #42: X objs, Y bytes @typeName]
3. alive 报告：格式同上

输出完成后，遍历所有会话表条目的 allocBucketRefs，设置
`prevCumFrees = cumFrees`、`prevCumFreeBytes = cumFreeBytes`，为下一轮 GC
的 freed delta 计算设好基线。

**Phase 6 — 写入输出。** 将 4MB 缓冲区的内容通过 write(2) 写入 stderr。
如果配置了 gcdeadtracefile，同步追加写入到文件。如果缓冲区溢出，写入
"..TRUNCATED" 标记。


最终用户看到的输出示例：

```
=== GC #1 ===
gcdeadsession by session:
  session #42: 1 allocs (256 bytes), 1 freed (256 bytes), 0 alive (0 bytes)
    [first: main.go:15 (gid=1), end: main.go:19]

gcdeadsession:freed: 1 session objs (256 bytes) freed from 1 sites
  main.main (main.go:15): 1 session objs, 256 session bytes
    [session #42: 1 objs, 256 bytes @[]uint8]

gcdeadsession:alive: 0 session objs (0 bytes) still alive from 0 sites
```


9.9 mProf_Malloc 和 gcDeadRecordFree 的对称性
------------------------------------------------

mProf_Malloc 和 gcDeadRecordFree 是 gcdeadtrace 数据采集的两个端点，它们操作的
是同一组计数器的不同子集。两者的更新内容对照如下：

**会话级计数：** mProf_Malloc 更新 e.allocs++、e.allocBytes += size、
e.cumAllocs++、e.cumAllocBytes += size。gcDeadRecordFree 完全不触及 allocs 系列
计数器，它更新 e.frees++、e.freeBytes += size、e.cumFrees++、
e.cumFreeBytes += size。分配和释放的计数器严格分离，不存在一个函数同时更新两边。

**goroutine 统计：** mProf_Malloc 更新 goroutineStats[gid].allocs++ 和
allocBytes。gcDeadRecordFree 更新 goroutineStats[gid].frees++ 和 freeBytes。
由于 goid 来自 special 中记录的值（而非当前释放的 goroutine），这个 freed 归属
准确地反映了分配时 goroutine 的释放量，即使释放由不同的 goroutine 执行。

**allocBucketRefs：** mProf_Malloc 更新 refs[j].frees++、refs[j].bytes += size，
并在首次插入时写入 typeName。gcDeadRecordFree 更新 refs[j].cumFrees++、
refs[j].cumFreeBytes += size。typeName 在分配时写入一次后不再修改。

**匹配条件不同：** mProf_Malloc 按 bucket 指针 + goid 双条件匹配，这是因为分配
时精确知道当前 goroutine。gcDeadRecordFree 仅按 bucket 指针匹配，不做 goid 过滤，
因为释放时不能假定只有分配时的 goroutine 能触发释放（对象可能被传递）。

**核心区分：** allocs 类计数器只增不减，仅由 mProf_Malloc 写入；frees 类计数器
也只增不减，仅由 gcDeadRecordFree 写入。两者之差（cumAllocs - cumFrees）就是
当前会话尚在堆上的存活对象数。这个公式是 gcDeadTracePrint Phase 1 中 alive 计数
的理论基础。


附录: GODEBUG 使用说明
=======================

A.1 基本用法
--------------

   // 完整模式：启用 gcdeadtrace，输出到 stderr
   GODEBUG=gcdeadtrace=1 go run main.go

   // 完整模式 + session 开始/结束日志 (gcdeadtrace=2)
   GODEBUG=gcdeadtrace=2 go run main.go

   // 轻量模式：开启会话机制，无输出（仅 gcdeadsession=1）
   GODEBUG=gcdeadsession=1 go run main.go

   // 同时输出到文件 (追加模式)
   GODEBUG=gcdeadtrace=1,gcdeadtracefile=gcdeadtrace.log go run main.go

   // 文件仅追加，不输出到 stderr
   GODEBUG=gcdeadtrace=1,gcdeadtracefile=gcdeadtrace.log go run main.go 2>/dev/null

A.2 输出格式
--------------

  仅输出 session 数据，无全局 gcdead: 报告:

   === GC #1 ===
   gcdeadsession by session:
     session #1002: 3 allocs (1304 bytes), 1 freed (256 bytes), 2 alive (1048 bytes)
       [first: main.go:361 (gid=18), end: main.go:372]

   gcdeadsession:freed: N session objs (M bytes) freed from K sites
     funcName (file:line): N session objs, M session bytes [session #N: X objs, Y bytes @typeName]

   gcdeadsession:alive: N session objs (M bytes) still alive from K sites
     funcName (file:line): N session objs, M session bytes [session #N: X objs, Y bytes @typeName]

  类型注解示例:
    @[]uint8        — byte slice
    @main.ListNode  — 自定义结构体指针
    @string         — 字符串
    @map[uint64][]uint8 — map 类型

A.3 Session API — 用户使用方式
--------------------------------

  用户代码只需无条件调用 Start/End，不需要条件编译或环境变量检查：

    runtime.GcDeadSessionStart(42)
    // ... 分配操作 ...
    runtime.GcDeadSessionEnd(42)

  所有行为通过 GODEBUG 控制，Start/End 内部自动判断：

  | 设置 | 行为 |
  |------|------|
  | 不设任何开关 | Start/End 直接返回，零开销 |
  | gcdeadsession=1 | 会话机制生效，追踪 start/end 位置，无输出 |
  | gcdeadtrace=1 | 全量采集 + GC 结束输出 + 强制 GC |

  Start/End 内部的三层保护：

    func GcDeadSessionStart(id uint64) {
        // 第一层：两个开关都没开 → 直接返回，零开销
        if debug.gcdeadsession == 0 && debug.gcdeadtrace == 0 { return }

        // 第二层：会话机制逻辑（gcdeadsession=1 或 gcdeadtrace=1 时执行）
        //   创建表条目、记录 startSites、设置 goroutine 字段、ended 检查

        // 第三层：追踪逻辑（仅 gcdeadtrace=1 执行）
        if debug.gcdeadtrace > 0 {
            // MemProfileRate=1、println 位置
        }
    }

  多 goroutine 用法:
    // goroutine A
    runtime.GcDeadSessionStart(42)
    // ... 分配 ...
    runtime.GcDeadSessionEnd(42)

    // goroutine B（同一 session）
    runtime.GcDeadSessionStart(42)
    // ... 分配 ...
    // B 不调 End，由 A 调 End 即可。End 后所有 goroutine 被清理，
    // B 可随后启动新 session: runtime.GcDeadSessionStart(43)

A.4 文件输出说明
------------------

  - gcdeadtracefile=<path> 追加模式，多次 GC 周期/多次运行不丢失
  - 文件不存在自动创建
  - 与 stderr 输出同时生效
  - 只输出 gcdeadtrace 内容，不包含普通程序日志

A.5 输出验证工具
------------------

  会话内存/demo/verify_freed.go — 验证 gcdeadtrace 输出的 per-site 字节总和
  是否与 summary 行一致：

    // 验证单文件
    go run verify_freed.go output/gcdeadtrace_concurrent_fixed.txt

    // 验证所有输出文件
    go run verify_freed.go

  验证逻辑：
    - 解析每个 GC 块的 gcdeadsession:freed/alive summary 行的字节数
    - 累加 per-site 行的字节数
    - 比较 summary 总计 vs per-site 总计
    - 报告 OK 或 MISMATCH
