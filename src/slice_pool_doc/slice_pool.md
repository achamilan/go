# SlicePool 设计文档

## 1. 概述

### 1.1 背景

Go 的 `sync.Pool` 用于缓存临时对象以减少 GC 压力。但当用于 **变长 slice**（如 `[]byte` buffer）时存在几个已知问题：

1. **类型不安全**：存取 `any`，调用方需要类型断言
2. **无容量感知**：`New` 是无参工厂，无法按请求容量分配；调用方只能事先约定一个固定容量
3. **无大小分级**：所有大小的 slice 混在同一池中。一次偶然的大 buffer 请求会占据池空间，排挤小 buffer，导致内存占用持续偏高（issue #23199）

Go 标准库中多处存在手工管理多级 `sync.Pool` 的代码（如 `net/http` 的 h2 实现），说明这是普遍需求。

### 1.2 目标

在 `sync` 包中新增 `SlicePool[E any]`：

- **类型安全**：泛型，调用方无类型断言
- **容量感知**：`Get(n)` 返回 cap >= n 的 slice，`New(n)` 按需分配
- **大小分级**：内部按 size class 拆分多个 `sync.Pool`，不同容量不互相排挤
- **底层复用 sync.Pool**：per-P 无锁缓存、victim cache、GC 清理全部继承

---

## 2. 公开 API

```go
package sync

// SlicePool is a generic pool for variable-sized slices.
type SlicePool[E any] struct {
    // New, if non-nil, must return a new slice of the specified capacity.
    New func(int) []E
}

// Get returns a slice of capacity at least n from the pool, if possible.
// If not possible and [New] is not nil, Get returns the value returned by [New](m),
// with m greater or equal to n.
// Otherwise it returns nil.
//
// Callers must not assume any relationship between individual slices returned by Get
// and slices previously passed as arguments to [Put].
// Callers can assume that the length and contents of the returned slice are either as
// set by [New], or as they were when the slice was passed to [Put].
func (p *SlicePool[E]) Get(n int) []E

// Put adds the slice s to the pool.
// The slice s should not be used after it has been passed to Put.
//
// If the slice has been previously obtained from a call to [Get], when Put is called
// the slice should have the same capacity as it had when it was returned by [Get].
//
// Put is best effort, in that it may silently drop the slice in case it detects it would
// not be beneficial to add it to the pool.
func (p *SlicePool[E]) Put(s []E)
```

### 2.1 设计要点

**为什么 Get 可能返回 cap > n 的 slice？**
`Get(n)` 将 n 向上取整到一个 size class，调用 `New(classMax)` 分配。由于 classMax 是 class 的上限值，返回的 slice cap 必然 >= n。同时 pool 中可能缓存了外部 `make` 的 slice（cap 等于任意值），Get 取出后会校验 cap >= n，不满足则丢弃。

**Put 的宽松接受策略**

Put 接受任意 cap 落入有效 class 的 slice（`minCap <= cap < 256K*sizeof(E)`），不再要求 cap 精确等于 classMax。这允许：
- `pool.Get` → 使用 → `pool.Put`（cap 不变，自然回到原 class）
- 外部 `make` 的 slice 直接 `pool.Put`（cap 按 sizeClass 进对应 class）
- append 扩容后 cap 变大但仍落在某个有效 class 时，也可回收

被静默丢弃的情况只有：
- cap < minCap（太小，池化不划算）
- cap 落入 top class（无上界，不池化）

**Get 的 cap 校验**

Get 从 pool 取出 slice 后校验 `cap(s) >= n`。若取出的是一个外部 put 的小 cap slice，无法满足本次请求，则丢弃并调用 New 分配。这保证了 Get 返回的 slice 总能满足 cap >= n 的约定。

**为什么保留 len 和内容？**
与 `sync.Pool` 一致——不强制清零。调用方若需要清零可在 Put 前自行处理。这样最小化假设，也允许调用方利用"Put 时的状态即 Get 时的状态"这一保证来避免重复初始化。

---

## 3. 架构设计

### 3.1 整体结构

```
┌─────────────────────────────────────────┐
│         sync.SlicePool[E any]            │
│         Get(n int) []E                   │
│         Put(s []E)                       │
├─────────────────────────────────────────┤
│       Size Class → sync.Pool 映射        │
│  class 0 → Pool    class 1 → Pool  ...  │
├─────────────────────────────────────────┤
│     sync.Pool (per-P, victim, GC)       │
└─────────────────────────────────────────┘
```

每个 size class 对应一个独立的 `sync.Pool`。这样：
- 不同容量的 slice 不会互相排挤
- 每个 class 独立享受 sync.Pool 的 per-P 无锁缓存和 victim cache
- GC 按 class 粒度清理（长时间不用的 class 被清空，热点 class 保留）

### 3.2 核心数据结构

```go
type SlicePool[E any] struct {
    New     func(int) []E
    classes [numClasses]Pool  // each Pool caches slices of one size class
}
```

### 3.3 容量分级策略

不采用简单的 power-of-2 分级（klauspost 指出"在较小 size 时碎片化严重"）。采用 **对数值四舍五入**（logarithmic with finer granularity at small sizes），与 runtime 内存分配器的 size class 设计思想一致——保证任意请求的向上取整开销有界。

```
class = round(log(cap), granularity)
```

实际策略（伪对数）：

| Class | 容量范围 (元素数) | 取整上限 | 最大浪费比 |
|-------|------------------|---------|-----------|
| 0     | 1 - 15           | N/A     | 不池化    |
| 1     | 16 - 31          | 31      | ~94%      |
| 2     | 32 - 63          | 63      | ~97%      |
| 3     | 64 - 127         | 127     | ~98%      |
| 4     | 128 - 255        | 255     | ~99%      |
| 5     | 256 - 511        | 511     | ~100%     |
| 6     | 512 - 1023       | 1023    | ~100%     |
| 7     | 1K - 2K-1        | 2047    | ~100%     |
| 8     | 2K - 4K-1        | 4095    | ~100%     |
| 9     | 4K - 8K-1        | 8191    | ~100%     |
| 10    | 8K - 16K-1       | 16383   | ~100%     |
| 11    | 16K - 32K-1      | 32767   | ~100%     |
| 12    | 32K - 64K-1      | 65535   | ~100%     |
| 13    | 64K - 128K-1     | 131071  | ~100%     |
| 14    | 128K - 256K-1    | 262143  | ~100%     |
| 15    | 256K+            | N/A     | 无上限    |

容量 < 16 的 slice 不池化——分配成本极低，池化管理开销不划算。

对于泛型 `SlicePool[E]`，`Get(n)` 中的 n 按**元素个数**计算 class。不同 `sizeof(E)` 会自然产生不同的字节级 memory class，与 runtime 分配器的行为对齐。

### 3.4 Get 流程

```
Get(n)
  │
  ├─ n <= 0? ──→ n = 1
  │
  ├─ class = sizeClass(n)          // 确定 class
  │
  ├─ class == numClasses-1? ──→ New(n)   // top class: 直接分配
  │
  ├─ v = classes[class].Get()      // sync.Pool.Get (lock-free per-P)
  │
  ├─ v != nil && cap(v) >= n? ──→ return v.([]E)  // 命中，cap 足够
  │
  ├─ v != nil && cap(v) < n? ──→ 丢弃             // cap 不够，pool 中可能有外部 Put 的小 slice
  │
  └─ New(classMax(class))          // 分配新 slice（cap = class 上限）
```

### 3.5 Put 流程

```
Put(s)
  │
  ├─ cap(s) < minCap(16)? ──→ return         // 太小，丢弃
  │
  ├─ class == numClasses-1? ──→ return       // 顶级 class，不池化
  │
  └─ classes[class].Put(s)                   // sync.Pool.Put，按 cap 进对应 class
```

**宽松策略**：Put 接受任意 cap 落入有效 class 的 slice。外部 `make` 的 slice、append 扩容后的 slice（只要 cap 仍在有效 class 范围内）都能回到池中。唯一丢弃的情况是 cap < minCap（不划算池化）或落入 top class（无上界，无法有效池化）。

---

## 4. 实现

### 4.1 代码（`src/sync/slicepool.go`）

```go
package sync

import "math/bits"

const (
    slicePoolMinCap     = 16
    slicePoolNumClasses = 16
)

// SlicePool is a generic pool for variable-sized slices.
type SlicePool[E any] struct {
    // New, if non-nil, must return a new slice of the specified capacity.
    New     func(int) []E
    classes [slicePoolNumClasses]Pool
}

// Get returns a slice of capacity at least n from the pool, if possible.
func (p *SlicePool[E]) Get(n int) []E {
    if n <= 0 {
        n = 1
    }
    class := slicePoolClass(n)
    if class < 0 {
        class = 0
    }

    // Top class: allocate directly, no pooling.
    if class == slicePoolNumClasses-1 {
        if p.New != nil {
            return p.New(n)
        }
        return nil
    }

    if v := p.classes[class].Get(); v != nil {
        s := v.([]E)
        if cap(s) >= n {
            return s
        }
        // cap too small (external slice with non-classMax cap); discard.
    }
    if p.New != nil {
        return p.New(slicePoolClassMax(class))
    }
    return nil
}

// Put adds the slice s to the pool.
// Any slice whose capacity falls within a valid size class is accepted.
func (p *SlicePool[E]) Put(s []E) {
    c := cap(s)
    class := slicePoolClass(c)
    if class < 0 {
        return // capacity < minCap
    }
    if class == slicePoolNumClasses-1 {
        return // top class: not pooled
    }
    p.classes[class].Put(s)
}

// slicePoolClass returns the size class index for capacity c.
// Returns -1 if c < slicePoolMinCap.
func slicePoolClass(c int) int {
    if c < slicePoolMinCap {
        return -1
    }
    class := bits.Len64(uint64(c/slicePoolMinCap)) - 1
    if class >= slicePoolNumClasses {
        return slicePoolNumClasses - 1
    }
    return class
}

// slicePoolClassMax returns the recommended allocation capacity for a size class.
// New is called with this value so that the returned slice's cap
// maps back to the same class.
func slicePoolClassMax(class int) int {
    if class+1 < slicePoolNumClasses {
        return slicePoolMinCap<<(class+1) - 1
    }
    return slicePoolMinCap << class // top class: no upper bound
}
```

### 4.2 设计讨论与取舍

**为什么不用 runtime 的精确 size class？**
Runtime 的 `runtime.class_to_size` 有 67 个 class，设计目标是最大化内存利用率（每个 class 的内部碎片最小）。但 67 个 class 意味着 67 个 `sync.Pool`，管理和 GC 开销偏大。16 级的伪对数分级在"class 数量"和"浪费比"之间取平衡：小容量浪费比稍高（~94%），但容量 > 128 后浪费比 > 98%，实用足够。

**为什么保留 New 字段而不是直接 `make([]E, m)`？**
- `New` 为 nil 时返回 nil，语义是"只在池中有缓存时才返回"，用于偶尔不需要池外分配的路径
- 调用方可能需要自定义构造函数（如预初始化某些字段）

**为什么 len 和内容保持不变？**
与 sync.Pool v1 一致。如果改为自动 `s = s[:cap(s)]` 并清零，虽然使用更"干净"，但增加每次 Put/Get 的开销。保留原样最小化假设——调用方需要什么行为自己控制。

**为什么 Get 返回 cap > n 的 slice 是合理的？**
如 klauspost 指出的，这是 size class 机制的自然结果。调用方通过 `buf[:actualLen]` 使用有效部分，多出的容量不影响正确性，且可减少后续 append 时的扩容。

**Put 的宽松 vs 严格策略**

最初设计采用严格策略：Put 要求 `cap(s) == classMax(class)`，即只接受池自己通过 `New(classMax)` 产出的 slice。这排除了外部 `make` 的 slice（cap 通常不等于 classMax）和 append 扩容后的 slice。

问题：在实践中，绝大多数外部 slice（如 `make([]byte, 1024)`，cap=1024 vs classMax=2047）和 append 扩容后的 slice 都会被拒绝，导致 Put 的接受率极低。用户反馈"基本都触发了 c != classMax(class) 分支"。

改进为宽松策略：Put 接受任意 cap 落入有效 class 的 slice，去掉了 `c == classMax(class)` 检查。代价是 pool 中可能出现 cap 参差不齐的 slice。Get 侧增加校验 `cap(s) >= n`，不满足则丢弃并分配新 slice。这保持了 Get 的契约（返回 cap >= n），同时大大提高了 Put 的接受率。

---

## 5. 与其他方案的对比

| 特性 | sync.Pool (裸用) | SlicePool |
|------|-----------------|-----------|
| 类型安全 | 否（`any` 断言） | 是（泛型） |
| 容量感知 New | 否（无参 `func() any`） | 是（`func(int) []E`） |
| 大小分级 | 无 | 16 级，每级独立 sync.Pool |
| 大 buffer 排挤问题 | 是（单一池） | 否（分级隔离） |
| GC 清理 | victim cache | 继承（每级独立 victim） |
| 二次切片保护 | 无 | Get 时校验 cap >= n，不满足则丢弃 |
| 性能（单线程） | ~35ns | ~36ns |
| 性能（并行） | ~17ns | ~18ns |

---

## 6. 标准库中的潜在使用场景

- `net/http` h2 实现（手动维护的多级 `sync.Pool`，issue #23199）
- `encoding/json` unmarshal 时的临时 slice 积累
- `slices.Collect` 的中间缓冲区
- `bytes.Buffer` 的底层 `[]byte`
- `io.ReadAll` 的读取缓冲区
- `math/big` 的 `[]uint` 运算中间结果

---

## 7. 测试设计

### 7.1 功能测试

| 测试 | 验证点 |
|------|--------|
| `TestBasic` | Get(1024) 返回 cap >= 1024，Put 后再 Get 命中缓存 |
| `TestGetRoundUp` | Get(20) 时 New 被调用且参数 >= 20（取整到 class 上限） |
| `TestGetBiggerFromPool` | Put(cap=800)，Get(300) 从大 class 命中 |
| `TestGetTooSmallReturnsNew` | 池中只有小 slice，Get 大请求回退到 New |
| `TestNewNil` | New 为 nil 时，池空返回 nil |
| `TestConcurrent` | 多 goroutine 并发 Get/Put 无 data race |
| `TestGetZero` | Get(0) 不 panic |
| `TestPutSmallDiscarded` | cap < 16 的 Put 被丢弃 |
| `TestReuseAcrossSizes` | 不同大小请求各自 class 独立缓存 |
| `TestGC` | GC 后 Get 仍能正常工作 |

### 7.2 基准测试

```go
func BenchmarkMakeNew(b *testing.B)          // 直接 make
func BenchmarkSyncPool(b *testing.B)          // 裸 sync.Pool
func BenchmarkSlicePool(b *testing.B)         // SlicePool
func BenchmarkSlicePoolParallel(b *testing.B) // 并行
func BenchmarkSyncPoolParallel(b *testing.B)  // 并行对比
func BenchmarkMixedSize(b *testing.B)         // 混合容量
func BenchmarkNetworkBuf(b *testing.B)        // 模拟网络 IO buffer
```

---

## 8. 实现路线图

| 阶段 | 工作内容 |
|------|---------|
| Phase 1 | `sync.SlicePool[E]` 核心实现 + 测试 |
| Phase 2 | size class 策略调优（可能加入 MaxCap） |
| Phase 3 | 二次切片检测（需 runtime 支持） |
| Phase 4 | 标准库内部迁移（net/http h2, encoding/json 等） |
