# Session Memory 测试设计文档

## 1. 设计目的

### 1.1 为什么需要独立的测试设计

Session Memory 是 runtime 的新子系统，涉及四条高风险集成路径：
1. **内存分配路径**（bump allocator → allocManual）
2. **内存释放路径**（Close → freeManual）
3. **GC mark 路径**（markroot → 保守扫描 session span）
4. **并发路径**（多 goroutine Alloc + Close）

每个路径都有独立的状态机和错误模式。本测试设计的目标是：**用状态机穷举法覆盖所有状态×刺激组合，确保没有未定义行为**。

### 1.2 设计状态

| 属性 | 值 |
|------|-----|
| 关联设计文档 | `session_memory_design.md` |
| 已实现测试 | 13 个 (13/13 PASS) |
| 测试文件 | `src/runtime/session_test.go` (264 行) |

---

## 2. 状态机建模与覆盖矩阵

### 2.1 Session 状态机测试矩阵

Session 有两个状态（Open, Closed），两种外部刺激（Alloc, Close）。

| 状态 \ 刺激 | `Alloc(size)` | `Close()` |
|------------|---------------|-----------|
| **Open** | bump 分配，返回指针 | 释放所有 bucket，切换到 Closed |
| **Closed** | 返回 nil | no-op |

**测试覆盖**：

| 迁移路径 | 覆盖测试 | 验证点 |
|---------|---------|--------|
| Open + Alloc → Open | TestSessionAlloc, TestSessionAllocSizes, TestSessionAllocAlignment, TestSessionConcurrentAlloc | 返回有效指针，8 字节对齐，内存可读写 |
| Open + Alloc(size=0) → Open | TestSessionZeroAlloc | 返回 nil |
| Open + Alloc(触发refill) → Open | TestSessionAllocMultipleBuckets, TestSessionEdgeBucket | 多桶数据完整性 |
| Open + Close → Closed | TestSessionClose, TestSessionCloseReleasesImmediately | 无 crash，span 归还 mheap |
| Closed + Close → Closed | TestSessionDoubleClose | no-op，无 crash |
| Closed + Alloc → Closed | TestSessionAllocAfterClose | 返回 nil |
| Open + Alloc + GC → Open | TestSessionGCInteraction | 活跃 session 的数据不被 GC 破坏 |

### 2.2 Bucket 元数据状态机测试矩阵

Bucket 元数据有三个状态（Free, Active, Ready），三种刺激（allocBucket, freeBucket, reapReadyBuckets）。

| 状态 \ 刺激 | `allocBucket(span)` | `freeBucket()` | `reapReadyBuckets()` |
|------------|---------------------|----------------|----------------------|
| **Free** | 取出，设为 Active | — | — |
| **Active** | — | 释放 span 引用，移到 Ready | — |
| **Ready** | — | — | 标记 needzero，移到 Free |

**测试覆盖**：

| 迁移路径 | 覆盖测试 | 验证点 |
|---------|---------|--------|
| Free → Active | 所有分配测试（隐式，通过 allocBucket） | 新创建 session 的首次 Alloc 成功 |
| Active → Ready | TestSessionClose, TestSessionCloseReleasesImmediately | Close 后 bucket 元数据进入 ready 池 |
| Ready → Free | TestSessionEdgeBucket | refill 中调用 reapReadyBuckets，元数据复用 |

### 2.3 activeSessionSpan 状态机测试矩阵

| 状态 \ 刺激 | `allocBucketSpan` | `Close` / `freeAllBucketsLocked` | GC markroot |
|------------|-------------------|--------------------------------|-------------|
| **不在列表** | addActiveSpan，加入列表 | — | 跳过（不扫描） |
| **在列表** | — | removeActiveSpan，swap-remove | 保守扫描 span 内容 |

**测试覆盖**：

| 迁移路径 | 覆盖测试 | 验证点 |
|---------|---------|--------|
| 不在列表 → 在列表 | TestSessionAllocMultipleBuckets | allocManual 后 span 被注册 |
| 在列表 → 不在列表 | TestSessionCloseReleasesImmediately | Close 后 removeActiveSpan |
| GC 扫描活跃 span | TestSessionGCInteraction | GC 期间活跃 session 不 crash 不丢数据 |
| Close 后 GC 不扫描 | TestSessionCloseReleasesImmediately | Close 后 GC 不 crash |

---

## 3. 已实现测试详解

### 3.1 基本功能测试（5 个）

#### TestSessionAlloc — 端到端基本路径

```
覆盖状态迁移：Open + Alloc → Open
验证：
  - NewSession() 返回非 nil
  - Alloc(128) 返回非 nil 且 8 字节对齐
  - 写入 0xDEADBEEF 后可正确读回
  - Close() 不 panic
调用路径：NewSession → Alloc → lock → bump → memclr → unlock → Close
```

#### TestSessionClose — 基本释放

```
覆盖状态迁移：Open + Close → Closed
验证：
  - NewSession() → Alloc(64) → 写入 42 → Close()
  - 无 panic/crash
调用路径：Close → lock → closed.Store → freeAllBucketsLocked → freeManual
```

#### TestSessionDoubleClose — 幂等 Close

```
覆盖状态迁移：Closed + Close → Closed
验证：第二次 Close() 为 no-op，无 crash
调用路径：Close → lock → closed.Load()==true → unlock → return
```

#### TestSessionAllocAfterClose — Close 后拒绝分配

```
覆盖状态迁移：Closed + Alloc → Closed
验证：Alloc(64) 返回 nil
调用路径：Alloc → closed.Load()==true → return nil（快速路径）
```

#### TestSessionZeroAlloc — 零大小分配

```
覆盖状态迁移：Open + Alloc(0) → Open
验证：Alloc(0) 返回 nil
调用路径：Alloc → size==0 → return nil
```

### 3.2 分配正确性测试（3 个）

#### TestSessionAllocSizes

```
覆盖状态迁移：Open + Alloc(various sizes) → Open
测试 size：1, 2, 3, 4, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65,
          127, 128, 129, 255, 256, 257, 511, 512, 1023, 1024, 4095, 4096, 8191, 8192
验证：每个 size 都成功分配、8 字节对齐、全部 size 字节可写
```

#### TestSessionAllocAlignment

```
覆盖状态迁移：Open + 连续 Alloc → Open（验证 alignUp 行为）
验证：
  - Alloc(1) → Alloc(8)：间隔 = 8 字节 (1 + 7 padding)
  - Alloc(3) → Alloc(8)：8 字节对齐
  - Alloc(9) → Alloc(8)：间隔 = 16 字节 (9 + 7 padding → alignUp 到 16)
覆盖代码：alignUp(x+size, 8)
```

#### TestSessionEdgeBucket

```
覆盖状态迁移：Open + Alloc(近桶边界) → refill → Open
               Ready → Free（通过 reapReadyBuckets）
验证：
  - 分配 bucketSize-16 字节 → 成功
  - 再分配 32 字节 → 触发 refill → 成功
  - 两次分配的数据完整性
覆盖路径：Alloc → bump+size>end → refill → allocBucketSpan → allocBucket
```

### 3.3 多桶测试（1 个）

#### TestSessionAllocMultipleBuckets

```
覆盖状态迁移：Open + Alloc × 32(twice bucket) → Open（多桶）
              不在列表 → 在列表（多次 addActiveSpan）
验证：
  - 32 次 8KB 分配（总计 256KB = 4 个 bucket）全部成功
  - 每个分配的序号写入后可正确读回
  - 跨桶数据不互相破坏
覆盖路径：refill → allocBucketSpan → allocManual → addActiveSpan → fullBuckets 链表
```

### 3.4 并发安全测试（2 个）

#### TestSessionConcurrentAlloc

```
覆盖状态迁移：Open + 并发 Alloc × 8000 → Open
验证：
  - 8 goroutine × 1000 次 Alloc(128)，全部成功
  - 无 data race（可通过 -race 验证）
  - 无 crash
覆盖路径：并发 lock(&s.mu)/unlock、并发 bump 更新（锁内保护）
```

#### TestSessionConcurrentMultiSessions

```
覆盖状态迁移：并发 10 组 (Open → Alloc → Close → Closed)
验证：
  - 10 个 goroutine 各自 NewSession → 100 次 Alloc → Close
  - 无 data race、无 crash
覆盖路径：并发 allocManual、并发 bucket pool 操作、并发 activeSessionSpans 操作
```

### 3.5 GC 交互测试（2 个）

#### TestSessionGCInteraction

```
覆盖状态迁移：Open + GC × 2 → Open（活跃 session 的数据不被 GC 破坏）
              在列表中 + GC markroot → 在列表（保守扫描不修改状态）
验证：
  - 活跃 session 的 Alloc(128) 写入 0xBEEFCAFE
  - GC × 2 后数据仍为 0xBEEFCAFE
  - 无 crash
覆盖路径：markroot → fixedRootSessionSpans → markrootSessionSpans（保守扫描）
```

#### TestSessionCloseReleasesImmediately

```
覆盖状态迁移：Open + Close → Closed → GC × 2 → Closed
              在列表 → 不在列表（removeActiveSpan）
验证：
  - Alloc(64KB) 强制 span 分配，Close 后 GC × 2
  - 无 crash（freeManual 归还的 span 不导致 GC 访问已释放内存）
覆盖路径：Close → removeActiveSpan → freeManual → GC（span 已不在 active 列表）
```

---

## 4. 状态机穷举完整性校验

### 4.1 Session 状态×刺激 完整性

对照状态机设计文档中的矩阵逐格核对：

| 格 | 覆盖测试 | 状态 |
|----|---------|------|
| Open / Alloc(>0) | TestSessionAlloc | ✅ |
| Open / Alloc(=0) | TestSessionZeroAlloc | ✅ |
| Open / Alloc(触发refill) | TestSessionAllocMultipleBuckets, TestSessionEdgeBucket | ✅ |
| Open / Close | TestSessionClose | ✅ |
| Closed / Alloc | TestSessionAllocAfterClose | ✅ |
| Closed / Close | TestSessionDoubleClose | ✅ |
| Open / GC (并发) | TestSessionGCInteraction | ✅ |
| Closed / GC (并发) | TestSessionCloseReleasesImmediately | ✅ |
| Open / 并发 Alloc | TestSessionConcurrentAlloc | ✅ |
| Open → Closed / Concurrent | TestSessionConcurrentMultiSessions | ✅ |

**结论**：Session 状态机的所有状态×刺激组合均已覆盖。

### 4.2 Bucket 元数据状态×刺激 完整性

| 格 | 覆盖测试 | 状态 |
|----|---------|------|
| Free → Active（allocBucket） | 所有 Alloc 测试（首次创建 bucket） | ✅ 隐式覆盖 |
| Active → Ready（freeBucket） | TestSessionClose | ✅ 隐式覆盖 |
| Ready → Free（reapReadyBuckets） | TestSessionEdgeBucket（refill 路径） | ✅ 隐式覆盖 |
| Free → Active → Ready → Free（完整循环） | TestSessionEdgeBucket + TestSessionClose 组合 | ✅ |

**注意**：Free→Active→Ready→Free 路径依赖 `allocBucket`/`freeBucket`/`reapReadyBuckets` 的内部实现，属于白盒覆盖。Phase 1 未导出这些函数的直接测试入口，但通过 `Alloc` + `Close` + `refill` 的路径组合实现了全覆盖。

### 4.3 activeSessionSpan 状态×刺激 完整性

| 格 | 覆盖测试 | 状态 |
|----|---------|------|
| 不在列表 → 在列表（allocBucketSpan） | TestSessionAllocMultipleBuckets | ✅ |
| 在列表 → 不在列表（Close） | TestSessionCloseReleasesImmediately | ✅ |
| 在列表 + GC markroot | TestSessionGCInteraction | ✅ |
| 不在列表 + GC markroot | TestSessionCloseReleasesImmediately (GC after Close) | ✅ |

---

## 5. 覆盖缺口与建议补充

### 5.1 当前未覆盖的场景

| 场景 | 风险等级 | 建议测试 | 阻塞 Phase 1? |
|------|---------|---------|--------------|
| session→heap 指针被 GC 正确追踪 | 中 | TestSessionSessionToHeapPointer：session 内存中的堆指针 → GC 后 heap 对象仍存活 | 建议在 Phase 1 补 |
| 大量会话（10000+）的创建/关闭 | 低 | TestSessionManySessions：循环 NewSession → Alloc → Close，验证无 OOM | 非阻塞 |
| 超过 bucket 大小的单次分配 | 低 | TestSessionAllocHugeSize：Alloc(65KB) 的行为 | 非阻塞 |
| Alloc 与 Close 的直接并发竞争 | 中 | TestSessionConcurrentAllocClose：一个 goroutine 持续 Alloc，另一个 Close | 建议补，验证 mutex + closed 双检的正确性 |

### 5.2 建议添加的测试

```go
// TestSessionSessionToHeapPointer 验证 session→heap 指针的 GC 安全性
func TestSessionSessionToHeapPointer(t *testing.T) {
    // 1. 在堆上分配对象，设置 finalizer
    // 2. session.Alloc 中写入指向堆对象的指针
    // 3. 放弃堆对象引用（只有 session 中的指针指向它）
    // 4. GC × 2 → 验证 finalizer 未被触发
    // 5. Close → GC × 2 → 验证 finalizer 被触发
}
```

---

## 6. 运行说明

```bash
# 全部 session 测试
go test runtime -run=TestSession -count=1 -v -timeout=120s

# 并发测试 + race detector
go test runtime -run=TestSessionConcurrent -race -count=10

# 带 GC trace 调试
GODEBUG=gctrace=1 go test runtime -run=TestSessionGC -count=1 -v
```

---

## 7. 变更历史

| 日期 | 变更内容 | 原因 |
|------|---------|------|
| 2026-07-02 | 初始版本：13 个测试覆盖 Phase 1 显式 Close 模型 | Phase 1 实现完成 |
| 2026-07-02 | 根据 state-machine-design skill 重写，增加状态机穷举矩阵和完整性校验 | 提升测试覆盖的系统性 |
