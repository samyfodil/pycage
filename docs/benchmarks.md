# Startup benchmarks

The command below separates cold native compilation, cached CPython instance
creation, and warm cell execution:

```console
make bench
```

These Go benchmarks intentionally leave `CompilationCacheDir` empty so the
cold case measures compilation instead of a prior process's disk-cache hit.

Use `-benchtime=1x` for cold benchmarks. A cold native compile is intentionally
expensive, and repeating it mostly measures CPU throttling and garbage
collection rather than normal application behavior.

## Baseline

Measured on 2026-08-02 on Linux/amd64 with an Intel Core i9-12900HK:

| Path | Wall time | Allocated bytes | Allocations |
| --- | ---: | ---: | ---: |
| Cold compiler sandbox | 19.81 s | 1.54 GB | 614k |
| Cold interpreter sandbox | 0.71 s | 510 MB | 215k |
| Cached compiler sandbox | 16.8-58.0 ms | 5.04 MB | 28.4k |
| Warm compiler `6 * 7` | 0.14-0.16 ms | 8-10 KB | 44-55 |
| Warm interpreter `6 * 7` | 2.15-2.26 ms | 6-8 KB | 45-56 |

Cold native compilation is sensitive to CPU frequency and ranged from 6.60 to
19.81 seconds across profiling and benchmark runs. The standalone CLI took
15.19-19.70 seconds in compiler mode and used 1.1-1.17 GB peak RSS. Interpreter
mode took 0.39 seconds with warm file caches (1.94 seconds on its first run) and
about 412 MB peak RSS.

With the persistent native cache enabled, an empty-cache compiler run took
13.76 seconds of setup and the identical command in a second process took
580 ms. Python execution itself remained below 1 ms in both runs.

These numbers are a diagnostic baseline, not portable performance claims.

## Bottleneck

A CPU profile of one cold compiler iteration attributes:

- 94.3% of samples to `Runtime.CompileModule`.
- 92.4% to compiling local Wasm functions in Wazy's native engine.
- 41.9% cumulatively to SSA optimization passes.
- 29.1% cumulatively to lowering Wasm to SSA.

The allocation profile attributes 81.6% of allocated space to the component
compile-cache miss path. Python evaluation is not the cold-start bottleneck.

## Choosing a mode

- CLI: keep the default compiler mode. The command stores Wazy's native
  compilation cache beneath the system temporary directory.
- Use `-runtime interpreter` explicitly only for a disposable invocation where
  an empty-cache cold start matters more than execution speed. Interpreter mode
  is never selected automatically.
- Service or agent: keep compiler mode and reuse one `pycage.Engine`. The first
  sandbox pays compilation; later isolated sandboxes reuse it.
- Reusing one stateful `Sandbox` is fastest when isolation between calls is not
  required.

Inspect a particular CLI invocation without an external profiler:

```console
./bin/pycage run -timing -runtime interpreter '6 * 7'
```

The timing line separates component setup from the exported WIT call.
