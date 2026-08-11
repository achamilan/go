# mempool（noscan span）设计文档

Go runtime 内的手动内存分配器，**单一职责：大的无指针对象的申请与释放**——
一对象一 span，Free 立即物理释放。内部实现位于 `src/runtime/mpoolspan.go`，
公共 API 为标准库包 `src/mempool`（经 linkname 绑定）。

## 1. 接口

```go
import "mempool"

// 原始字节 buffer
p := mempool.AllocSpan(size)   // 一对象一 span；内存已清零；noscan
mempool.FreeSpan(p)            // 立即还物理内存；FreeSpan(nil) 是 no-op

// 无指针复杂类型（泛型封装，带约束检查）
type Header struct { Magic uint32; Pad [64 << 10]byte }
h := mempool.AllocObject[Header]()  // T 含指针时 panic
h.Magic = 0xDEADBEEF
mempool.FreeObject(h)
```

调用约定（C 语义，违反即未定义行为）：

- **内存不被 GC 扫描（noscan），禁止存放 Go 指针**（指向的对象会被回收）；
  `AllocObject` 在分配时检查 `T.Pointers()`，含指针直接 panic
- Free 后访问**会段错误**（sysFault 的 fault 页，这是特性）
- 重复 Free / Free 非本包指针：未定义行为
- 适用：>1KB 的纯字节/纯值数据（IO buffer、序列化缓冲、无指针结构体等）；
  小于 1KB 或含指针的对象请用普通 Go 分配 / sync.Pool

## 2. 架构

`(*mheap).allocUserArenaSpan(npages)`——官方 userArena chunk 机制的
**可变大小 + noscan** 泛化（对 arena.go 的改动：两个固定 8MB 尺寸检查移除、
`allocUserArenaChunk` 的 readyList 跳过非匹配尺寸 span）：

```
AllocSpan(size)
  ├─ npages = alignUp(size, 4KB)          # noscan：无位图保留区，整 span 可用
  ├─ 快路径：延迟 fault 缓存命中 → memset + 记账（无页系统调用）
  └─ 慢路径：readyList 精确尺寸复用 / sysAlloc 新分配
FreeSpan(p)
  ├─ 缓存有位 → 入缓存（零页操作）
  └─ 否则 → freeUserArenaChunk：GC 相位允许时立即 sysFault 还 OS，
            quarantine → sweeper → readyList（地址空间复用）
```

### 2.1 延迟 fault 缓存

- `mpSpanCache`：32 条上限，按 npages 分键；命中路径 ~µs 级（vs 未命中 ~196µs 页操作）
- **正确性**：缓存 span 仍是 mSpanInUse 且不被应用引用，sweeper 本想回收它——
  但缓存用 Go map 持有 span 基址（GC 扫描容器），GC 每轮标记它，sweep 见到
  nalloc>0 即保留（与 userArena 的 refs 同构）
- `mpSpanCacheFlush` 在 gcStart（STW）drain，保证新一轮 sweep 之前没有
  "活着但无引用"的缓存 span
- 缓存命中跳过 profilealloc（避免 profile bucket 冲突；命中分配对内存
  profiler 不可见，属已文档化的近似）

### 2.2 noscan 特化的两个要点

1. 无位图保留区（scan 版需 spanBytes/64 + dummy type），整 span 可用
2. 跳过 `initHeapBits`（零长位图区，否则越界写）与 `largeType` 假类型初始化
   （dummy type 驻留在保留区里；mspan 结构体复用，需置 nil 防残留指针）

## 3. 性能（go1.24.12，i7-1165G7）

固定尺寸集压测（64/128/256/512KB，32 workers，ring 驻留，逐页写入，2s/模式）：

| 模式 | 吞吐 | 峰值 HeapInuse |
|---|---|---|
| sync.Pool | 9.7M ops/s | 132MB |
| **AllocSpan/FreeSpan** | **146K ops/s** | **51MB** |
| make | 95K ops/s | 734MB |

随机尺寸（64KB~2MB，缓存命中率低）：~8K ops/s，峰值 ~204MB（make：20.6K，1.2GB）。

结论：尺寸聚簇（真实 buffer 池形态）时吞吐**反超 make** 且内存低一个数量级；
尺寸高度分散时吞吐吃亏但内存始终贴近活水位。

## 4. 验证

- `runtime` 测试：`TestMPNoscan*`（基本/跨 GC 数据完好/复用/并发）+
  两个高并发压测（随机尺寸 / 固定尺寸集）
- 公共包：`src/mempool/mempool_test.go`（linkname 端到端）
- `TestLockRank`、`go test go/build`（deps 白名单）
- `go test runtime -short` 全套通过

## 5. 构建与测试命令（本树）

```bash
# 首次构建工具链（git-bash 无法直接跑 make.bat，直接调 dist）
cd src
GO111MODULE=off GOENV=off /d/go1.25.8.windows-amd64/go/bin/go.exe build -o cmd/dist/dist.exe ./cmd/dist
GOROOT='D:\code\go\github_go\go' GOROOT_BOOTSTRAP='D:\go1.25.8.windows-amd64\go' ./cmd/dist/dist.exe bootstrap -a

# 日常迭代（go test 自动重建 runtime）
GOROOT='D:\code\go\github_go\go' GOTOOLCHAIN=local ../bin/go.exe test runtime -run 'TestMP' -v
GOROOT='D:\code\go\github_go\go' GOTOOLCHAIN=local ../bin/go.exe test mempool -v
```

## 6. 文件索引

| 文件 | 职责 |
|---|---|
| `src/runtime/mpoolspan.go` | noscan span 分配/释放 + 延迟 fault 缓存 + allocUserArenaSpan |
| `src/runtime/mpoollink.go` | linkname 包装（对 src/mempool 发射符号） |
| `src/runtime/mpoolnoscan_test.go` | 测试与压测 |
| `src/runtime/arena.go`（改） | userArena 机制可变大小泛化（3 处） |
| `src/runtime/mgc.go`（改） | gcStart 挂 mpSpanCacheFlush |
| `src/runtime/mklockrank.go`/`lockrank.go`（改） | mpArena rank 登记 |
| `src/mempool/mempool.go` | 公共 API（AllocSpan/FreeSpan） |
| `src/mempool/mempool_test.go` | 公共路径端到端测试 |
| `src/go/build/deps_test.go`（改） | 依赖白名单（`unsafe < mempool`） |
| `api/next/mempool.txt` / `doc/next/6-stdlib/mempool.md` | API 列表 / 发行说明 |

## 7. 已知限制

- 缓存按精确 npages 命中，尺寸高度分散的负载命中率低
- 缓存命中对内存 profiler 不可见（近似）
- `-race` 需 cgo（本机 Windows 不可用），并发正确性靠压测覆盖
