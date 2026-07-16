# gcdeadtrace Session 模型改造：基于 sessionId 的设计

---

## 1. 背景

当前 gcdeadtrace 的 session 模型以 goroutine 为粒度：

```go
func GcDeadSessionStart()          // 自动分配 sessionID，绑定到当前 goroutine
func GcDeadSessionEnd()            // 结束当前 goroutine 的 session
```

问题：
- 一个逻辑 session 可能跨多个 goroutine（如连接池、Actor 模式），各 goroutine 的分配无法归到同一 session
- sessionID 由运行时自动生成，业务无法控制

需求：
- 业务传入 sessionId，相同 sessionId 的分配归到同一 session
- 一个 sessionId 可被多个 goroutine 同时使用
- Start 可多次调用（如多个 goroutine 加入同一 session）
- End 只能调用一次（表示该 session 结束，不再接受新分配）

---

## 2. API 变更

```go
// Before (goroutine-local session):
func GcDeadSessionStart()
func GcDeadSessionEnd()

// After (explicit sessionId):
func GcDeadSessionStart(id uint64)
func GcDeadSessionEnd(id uint64)
```

**Start 语义**：
- 调用 goroutine 加入 sessionId 标识的 session
- 同一 goroutine 重复调用相同 sessionId → 幂等，无效果
- 不同 goroutine 调用相同 sessionId → 各自加入同一 session
- 如果 session 已结束（End 已调用）→ 当前调用无效（不加入已结束的 session）

**End 语义**：
- 标记 sessionId 的 session 为"已结束"
- 只能调用一次（重复调用无效）
- 已结束的 session 不再接受新分配的归属
- 已分配但尚未 GC 的对象释放时仍能通过 special 回写 freed 数据

---

## 3. 核心改动

### 3.1 Goroutine 字段调整

保留 goroutine 级别的 session 字段，但改为由显式 sessionId 驱动：

```go
// runtime2.go — goroutine struct (不变)
gcDeadSessionActive bool
gcDeadSessionID uint64
```

- Start(id) 设置 `gcDeadSessionID = id`, `gcDeadSessionActive = true`
- 多个 goroutine 可能持有相同 `gcDeadSessionID`
- End(id) 不清除其他 goroutine 的字段（只标记 session 表条目已结束）

### 3.2 Session 表条目新增字段

```go
// mprof.go — gcDeadSessionInfo 新增
type gcDeadSessionInfo struct {
    id         uint64
    goid       uint64   // 创建该 session 的 goroutine ID（用于调试参考）
    startPC    uintptr
    endPC      uintptr
    ended      bool     // ← 新增：End 已调用
    printed    uint32
    // ... 其余字段不变 ...
}
```

`ended` 字段的作用：
- Start(id) 时检查 `e.ended == true` → 不加入已结束的 session
- mProf_Malloc 中检查 session 的 `ended` 标志 → 已结束的 session 不归属新分配

### 3.3 Start 实现

```go
func GcDeadSessionStart(id uint64) {
    if debug.gcdeadtrace == 0 { return }
    gp := getg().m.curg
    if gp == nil || gp.gcDeadSessionActive { return }

    idx := id % gcDeadMaxSessions
    e := &gcDeadSessionTable[idx]

    // 如果 session 已结束，不接受新加入
    if e.ended { return }

    // 首次创建 session 条目
    if e.id != id {
        e.id = id
        e.goid = gp.goid
        e.startPC = sys.GetCallerPC()
        e.endPC = 0
        e.ended = false
        e.printed = 0
        // 重置计数（如果是被覆盖的旧条目）
    }

    gp.gcDeadSessionActive = true
    gp.gcDeadSessionID = id

    // MemProfileRate 管理不变
    if gcDeadSessionCount.Add(1) == 1 {
        gcDeadSavedRate = MemProfileRate
        MemProfileRate = 1
    }
}
```

关键变更：
- sessionID 从 `gcDeadNextSessionID.Add(1)` 改为参数 `id`
- 新增 `e.ended` 检查
- 同一个 sessionId 被多个 goroutine 调用时，每个 goroutine 都会增加 `gcDeadSessionCount`（参考计数是 per-goroutine 的）

### 3.4 End 实现

```go
func GcDeadSessionEnd(id uint64) {
    if debug.gcdeadtrace == 0 { return }
    gp := getg().m.curg
    if gp == nil || !gp.gcDeadSessionActive { return }

    idx := id % gcDeadMaxSessions
    e := &gcDeadSessionTable[idx]

    // 只能 End 一次
    if e.ended { return }

    // 记录结束位置
    e.endPC = sys.GetCallerPC()
    e.ended = true   // 标记结束

    // 清除当前 goroutine 的 session 状态
    gp.gcDeadSessionActive = false
    gp.gcDeadSessionID = 0

    // 减去所有参与该 session 的 goroutine 的计数
    // 注意：这里需要知道有多少 goroutine 加入了该 session
    // TODO: 需要 track 参与 goroutine 数量
}
```

问题：`gcDeadSessionCount` 是全局活跃 goroutine 数（用于 MemProfileRate 管理）。如果 sessionId=42 有 5 个 goroutine 加入，那么 `gcDeadSessionCount` 会被增加 5 次。End(42) 需要减去所有 5 个计数才合理。

但 End 只调用一次，所以需要一个**参与 goroutine 计数**来正确处理 MemProfileRate。

方案：在 session 表条目中增加 `joinCount` 字段：
```go
type gcDeadSessionInfo struct {
    // ...
    joinCount int32  // 加入该 session 的 goroutine 数
}
```

- Start(id) → `atomic.Xadd(&e.joinCount, 1)`, `gcDeadSessionCount++`
- End(id) → 使用 `e.joinCount` 一次性减去对应数量的 `gcDeadSessionCount`

### 3.5 mProf_Malloc 中的分配归属

当前逻辑（mprof.go:469-513）：

```go
if debug.gcdeadtrace > 0 {
    gp := mp.curg
    if gp != nil && gp.gcDeadSessionActive {
        sid := gp.gcDeadSessionID
        idx := sid % gcDeadMaxSessions
        e := &gcDeadSessionTable[idx]
        if e.id == sid {
            // 更新 alloc 计数
        }
    }
}
```

需要新增检查：如果 session 已 ended，不归属：

```go
if gp != nil && gp.gcDeadSessionActive {
    sid := gp.gcDeadSessionID
    idx := sid % gcDeadMaxSessions
    e := &gcDeadSessionTable[idx]
    if e.id == sid && !e.ended {  // ← 新增 !e.ended
        // 更新 alloc 计数
    }
}
```

Special 挂载不变：仍使用 `gp.gcDeadSessionID`。

### 3.6 gcDeadRecordFree（不变）

`gcDeadRecordFree` 通过 special 中的 sessionID 查找 session 表条目并更新 freed 计数。即使 session 已 ended，pending 的 special 释放时仍能正确回写。**不变。**

### 3.7 输出格式调整

当前：
```
session #7 (gid=27): 4 allocs (1792 bytes), 0 freed (0 bytes), 4 alive (1792 bytes) [start: main.go:405, end: main.go:410]
```

改为使用用户提供的 sessionId：
```
session #42: 4 allocs (1792 bytes), 0 freed (0 bytes), 4 alive (1792 bytes) [start: main.go:405, end: main.go:410]
```

- 去掉 `(gid=N)`，因为 session 不再绑定单个 goroutine
- 显示原始 sessionId（而不是自动编号）

---

## 4. 影响范围

| 文件 | 改动 |
|------|------|
| `src/runtime/runtime2.go` | `goroutine` 字段不变（保持 gcDeadSessionActive/gcDeadSessionID） |
| `src/runtime/mprof.go` | GcDeadSessionStart/End 函数重写；session 表新增 ended/joinCount；输出格式微调 |
| `src/runtime/mheap.go` | specialGcDeadSession 结构体不变 |
| `src/runtime/testdata/testprog/gc.go` | 测试程序调用处加 sessionId 参数 |

---

## 5. 边界场景

| 场景 | 行为 |
|------|------|
| Goroutine A: Start(1) → Goroutine B: Start(1) → A 和 B 各自分配 | 都归 session 1 |
| Goroutine A: Start(1) → Start(1) (重复调用) | 幂等，第二次无效果 |
| Goroutine A: Start(1) → End(1) → Goroutine B: Start(1) | B 无法加入（session 已 ended） |
| End(1) 后还有 A 的 pending 对象释放 | gcDeadRecordFree 仍能回写（special 独立于 ended 标志） |
| sessionId 哈希冲突（id%4096 相同但 id 不同） | 现有 `e.id == sid` 检查机制不变，安全 |

---

## 6. 待决策项

| 问题 | 选项 |
|------|------|
| End 是否需要等待所有 goroutine 的 pending 分配完成？ | 不需要 — 分配是瞬时操作；挂 special 时已捕获 sessionId |
| sessionId 是 uint64 够用吗？ | 业务通常用 uint64 作为连接/用户标识；也可考虑支持 string（复杂度高，暂不推荐） |
| 已 ended 的 session 表条目何时清空？ | 当前不清空（与现有机制一致：条目被新 session 覆盖才失效）；可考虑在 GC 确认无 pending special 后清空 |
| joinCount 是否需要 atomic？ | 需要，多个 goroutine 可能并发 Start/End |
| 输出中 (gid=N) 去掉后是否保留创建者 goid 作为参考？ | 建议保留在调试输出中，不在常规 per-session 行显示 |
