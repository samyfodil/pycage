package pycage

import (
	"context"
	"testing"
)

// BenchmarkColdSandboxCompiler measures the default one-shot CLI path:
// create a runtime, decode and compile CPython, initialize it, then tear down.
// Run with -benchtime=1x: each iteration is intentionally expensive.
func BenchmarkColdSandboxCompiler(b *testing.B) {
	benchmarkColdSandbox(b, DefaultConfig())
}

// BenchmarkColdSandboxInterpreter measures the lower-latency one-shot path.
func BenchmarkColdSandboxInterpreter(b *testing.B) {
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	benchmarkColdSandbox(b, config)
}

func benchmarkColdSandbox(b *testing.B, config Config) {
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		sandbox, err := New(ctx, config)
		if err != nil {
			b.Fatal(err)
		}
		if err := sandbox.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCachedSandbox measures creating a fresh, isolated CPython instance
// after the Engine has cached component decoding and native compilation.
func BenchmarkCachedSandbox(b *testing.B) {
	ctx := context.Background()
	engine, err := NewEngine(ctx, DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close(ctx)

	warmup, err := engine.NewSandbox(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if err := warmup.Close(ctx); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sandbox, err := engine.NewSandbox(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if err := sandbox.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWarmRunCode isolates Python execution and component ABI overhead
// after both Wazy and CPython are initialized.
func BenchmarkWarmRunCodeCompiler(b *testing.B) {
	benchmarkWarmRunCode(b, DefaultConfig())
}

func BenchmarkWarmRunCodeInterpreter(b *testing.B) {
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	benchmarkWarmRunCode(b, config)
}

func benchmarkWarmRunCode(b *testing.B, config Config) {
	ctx := context.Background()
	sandbox, err := New(ctx, config)
	if err != nil {
		b.Fatal(err)
	}
	defer sandbox.Close(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := sandbox.RunCode(ctx, `6 * 7`)
		if err != nil {
			b.Fatal(err)
		}
		if result.Text() != "42" {
			b.Fatalf("unexpected result: %q", result.Text())
		}
	}
}
