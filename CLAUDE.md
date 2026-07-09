# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

This is the **Go programming language** source repository. The canonical repo is at `go.googlesource.com/go`. The `src/` directory contains the Go standard library, compiler toolchain, runtime, and the `go` command itself. The module name is `std` (see `src/go.mod`).

The build process uses **bootstrap**: an existing Go installation (>= Go 1.24.6) builds `cmd/dist`, which then bootstraps the full toolchain. `GOROOT_BOOTSTRAP` must point to this bootstrap toolchain and must not equal `$GOROOT`.

## Build commands

On Unix:
```
cd src && ./make.bash          # build the toolchain (compiler, linker, go command)
cd src && ./all.bash           # build + run all tests
```

On Windows:
```
cd src && make.bat             # build
cd src && all.bat              # build + test
```

The build installs binaries to `$GOROOT/bin/`. After building, add `$GOROOT/bin` to `PATH` so subsequent `go` commands use the newly built toolchain.

To clean: `cd src && ./clean.bash` (Unix) or `clean.bat` (Windows).

## Test commands

Run all toolchain + runtime tests (requires a built toolchain):
```
cd src && ../bin/go tool dist test -rebuild
```

Run only `cmd/go` tests:
```
go test cmd/go
```

Run a single `cmd/go` script test (scripts live in `src/cmd/go/testdata/script/`):
```
go test cmd/go -run=Script/^name_of_test$
```

Run toolchain/runtime tests from the top-level `test/` directory:
```
go test cmd/internal/testdir                          # all tests
go test cmd/internal/testdir -run='Test/(file1.go|file2.go)'   # specific files
```

Run tests for a specific package (e.g. compiler, runtime, standard library):
```
go test ./...                      # within a package directory
go test cmd/compile/internal/ssa   # specific internal package
```

Run compiler tests with debugging flags:
```
go build -gcflags=-m=2                  # print inlining/escape analysis
GOSSAFUNC=Foo go build                  # generate ssa.html for function Foo
go build -gcflags=-S                    # print assembly
go build -gcflags=-d=ssa/check_bce/debug  # bounds check elimination info
```

## High-level architecture

### Source layout (`src/`)

| Path | Purpose |
|------|---------|
| `src/cmd/go/` | The `go` command (build, test, mod, etc.) |
| `src/cmd/compile/` | The Go compiler ("gc") — see `src/cmd/compile/README.md` for phase details |
| `src/cmd/link/` | The linker |
| `src/cmd/asm/` | The assembler |
| `src/cmd/dist/` | Bootstrap tool; orchestrates the build |
| `src/cmd/internal/` | Packages shared by toolchain commands (obj, dwarf, src, etc.) |
| `src/runtime/` | The Go runtime (scheduler, GC, memory allocator) — see `src/runtime/HACKING.md` |
| `src/internal/` | Internal packages used by the standard library (e.g. `goarch`, `goos`, `cpu`, `race`) |
| `src/go/types/` | Go type checker (used by tools like gopls, NOT by the compiler directly) |
| `test/` | Toolchain and runtime regression tests |

### Compiler phases (in order)

1. **Parsing** — `cmd/compile/internal/syntax`
2. **Type checking** — `cmd/compile/internal/types2`
3. **IR construction ("noding")** — `cmd/compile/internal/noder`, `ir`, `types` (converts from syntax/types2 to compiler's own AST)
4. **Middle end** — `inline`, `devirtualize`, `escape` (optimization passes on IR)
5. **Walk** — `walk` (desugars complex statements into simpler primitives)
6. **SSA** — `ssagen` (IR→SSA conversion), then `ssa` (machine-independent + arch-specific passes)
7. **Code generation** — `cmd/internal/obj` (SSA→machine code)

The compiler uses its own AST (`ir`) and type system (`types`), separate from `go/ast` and `go/types`. `go/types` (and `cmd/compile/internal/types2`) are separate; the compiler converts from types2→`types` during noding.

### Runtime (G-M-P model)

- **G**: goroutine (type `g`)
- **M**: OS thread (type `m`)
- **P**: processor — resources to execute Go code (type `p`); there are exactly `GOMAXPROCS` Ps

The scheduler matches Gs (code), Ms (execution threads), and Ps (resources). Use `getg().m.curg` for the current user goroutine. `getg()` alone returns the current g, which may be g0 (system stack) or gsignal on signal stacks. See `src/runtime/HACKING.md` for conventions on nosplit, atomics, synchronization, write barriers, and linknames.

## Testing conventions

- **cmd/go tests**: Script-based tests in `src/cmd/go/testdata/script/*.txt`. Each file uses the `txtar` archive format with a command script followed by file contents. See the README in that directory for the full command reference.
- **Compiler/runtime tests**: Live in the top-level `test/` directory, run via `cmd/internal/testdir`.
- **Standard library tests**: Regular Go tests within each package.
- **API stability**: `api/next/` tracks new public API. Files in `doc/next/` must correspond. Do not add `RELNOTE=yes` comments in CLs; add files to `doc/next/` instead.

## Commit message style

Conventional format: `package/path: short description`. Examples:
- `cmd/go: add VCS telemetry counter`
- `bytes,slices,strings: ContainsFunc: document short-circuit semantics`
- `net/http: use net/http/internal/http2 rather than h2_bundle.go`

## Linkname conventions

Three standard forms of `//go:linkname`:
- **Push**: definition pushes to another package's symbol name
- **Pull**: declaration pulls from another package's symbol
- **Export**: `//go:linkname` with no second argument; marks a symbol as available for other packages to link against

Always prefer push linknames. The linker (as of Go 1.23) forbids pull linknames of stdlib symbols unless they participate in a handshake (push + export pair).
