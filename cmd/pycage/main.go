package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samyfodil/pycage"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		usage()
		os.Exit(2)
	}

	flags := flag.NewFlagSet("pycage run", flag.ExitOnError)
	timeout := flags.Duration("timeout", 5*time.Second, "maximum execution time")
	memory := flags.Uint64("memory", 256<<20, "memory limit in bytes")
	runtimeMode := flags.String("runtime", "compiler", "Wazy runtime: compiler or interpreter")
	cacheDir := flags.String("cache-dir", defaultCacheDir(), "native compilation cache directory")
	allowNetwork := flags.Bool("network", false, "allow outbound TCP from the sandbox")
	jsonOutput := flags.Bool("json", false, "print the complete result as JSON")
	showTiming := flags.Bool("timing", false, "print sandbox setup and execution timings")
	var wheels stringList
	var requirements stringList
	flags.Var(&wheels, "wheel", "pure-Python wheel to install (repeatable)")
	flags.Var(&requirements, "pip", "requirement to install with embedded pip (repeatable)")
	_ = flags.Parse(os.Args[2:])
	if flags.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "pycage: missing Python code")
		os.Exit(2)
	}

	ctx := context.Background()
	setupStarted := time.Now()
	sandbox, err := pycage.New(ctx, pycage.Config{
		Timeout:             *timeout,
		MemoryLimitBytes:    *memory,
		RuntimeMode:         pycage.RuntimeMode(*runtimeMode),
		CompilationCacheDir: *cacheDir,
		AllowNetwork:        *allowNetwork,
	})
	if err != nil {
		fatal(err)
	}
	setupElapsed := time.Since(setupStarted)
	defer sandbox.Close(ctx)
	for _, wheelPath := range wheels {
		wheel, err := os.ReadFile(wheelPath)
		if err != nil {
			fatal(fmt.Errorf("read wheel %q: %w", wheelPath, err))
		}
		if _, err := sandbox.InstallWheel(wheel); err != nil {
			fatal(fmt.Errorf("install wheel %q: %w", wheelPath, err))
		}
	}
	if len(requirements) > 0 {
		result, err := sandbox.PipInstall(ctx, requirements...)
		if err != nil {
			fatal(err)
		}
		fmt.Print(result.Stdout)
		fmt.Fprint(os.Stderr, result.Stderr)
		if result.ExitCode != 0 || result.Error != "" {
			fatal(fmt.Errorf("pip failed with exit code %d: %s", result.ExitCode, result.Error))
		}
	}

	executionStarted := time.Now()
	execution, err := sandbox.RunCode(ctx, strings.Join(flags.Args(), " "))
	executionElapsed := time.Since(executionStarted)
	if err != nil {
		fatal(err)
	}
	if *showTiming {
		fmt.Fprintf(os.Stderr, "pycage timing: setup=%s execution=%s\n", setupElapsed, executionElapsed)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(execution); err != nil {
			fatal(err)
		}
		return
	}

	fmt.Print(execution.Stdout)
	fmt.Fprint(os.Stderr, execution.Stderr)
	if text := execution.Text(); text != "" {
		fmt.Println(text)
	}
	if execution.Error != nil {
		fmt.Fprint(os.Stderr, execution.Error.Traceback)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: pycage run [options] 'python code'

Options:
  -timeout 5s       maximum execution time
  -memory 268435456 memory limit in bytes
  -runtime compiler  Wazy runtime: compiler or interpreter
  -cache-dir path     native cache (default: temporary directory)
  -network            allow outbound TCP from the sandbox
  -timing             print setup and execution timings
  -wheel path       install a pure-Python wheel (repeatable)
  -pip requirement  install with embedded pip (repeatable)
  -json             print structured execution JSON`)
}

func defaultCacheDir() string {
	return filepath.Join(os.TempDir(), "pycage", "wazy-native")
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
