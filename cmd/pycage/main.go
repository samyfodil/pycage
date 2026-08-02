package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	jsonOutput := flags.Bool("json", false, "print the complete result as JSON")
	var wheels stringList
	flags.Var(&wheels, "wheel", "pure-Python wheel to install (repeatable)")
	_ = flags.Parse(os.Args[2:])
	if flags.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "pycage: missing Python code")
		os.Exit(2)
	}

	ctx := context.Background()
	sandbox, err := pycage.New(ctx, pycage.Config{
		Timeout:          *timeout,
		MemoryLimitBytes: *memory,
	})
	if err != nil {
		fatal(err)
	}
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

	execution, err := sandbox.RunCode(ctx, strings.Join(flags.Args(), " "))
	if err != nil {
		fatal(err)
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
  -wheel path       install a pure-Python wheel (repeatable)
  -json             print structured execution JSON`)
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
