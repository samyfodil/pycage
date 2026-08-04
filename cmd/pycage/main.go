package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samyfodil/pycage"
	"github.com/spf13/afero"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCommand(os.Args[2:])
	case "serve":
		serveCommand(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runCommand(argv []string) {
	flags := flag.NewFlagSet("pycage run", flag.ExitOnError)
	timeout := flags.Duration("timeout", 5*time.Second, "maximum execution time")
	memory := flags.Uint64("memory", 256<<20, "memory limit in bytes")
	runtimeMode := flags.String("runtime", "compiler", "Wazy runtime: compiler or interpreter")
	cacheDir := flags.String("cache-dir", defaultCacheDir(), "native compilation cache directory")
	allowNetwork := flags.Bool("network", false, "allow outbound TCP and HTTP(S) from the sandbox")
	jsonOutput := flags.Bool("json", false, "print the complete result as JSON")
	showTiming := flags.Bool("timing", false, "print sandbox setup and execution timings")
	var wheels stringList
	var requirements stringList
	var binds stringList
	var cowBinds stringList
	flags.Var(&wheels, "wheel", "pure-Python wheel to install (repeatable)")
	flags.Var(&requirements, "pip", "requirement to install with embedded pip (repeatable)")
	flags.Var(&binds, "bind", "bind host directory read-write as host[=guest] (repeatable)")
	flags.Var(&cowBinds, "bind-cow", "bind host directory with an in-memory COW layer as host[=guest] (repeatable)")
	flags.Parse(argv)
	if flags.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "pycage: missing Python code")
		os.Exit(2)
	}

	ctx := context.Background()
	setupStarted := time.Now()
	filesystem, err := bindingFileSystem(binds, cowBinds)
	if err != nil {
		fatal(err)
	}
	sandbox, err := pycage.New(ctx, pycage.Config{
		Timeout:             *timeout,
		MemoryLimitBytes:    *memory,
		RuntimeMode:         pycage.RuntimeMode(*runtimeMode),
		CompilationCacheDir: *cacheDir,
		AllowNetwork:        *allowNetwork,
		FileSystem:          filesystem,
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

func serveCommand(argv []string) {
	flags := flag.NewFlagSet("pycage serve", flag.ExitOnError)
	addr := flags.String("addr", fmt.Sprintf("127.0.0.1:%d", pycage.DefaultServerPort), "listen address")
	token := flags.String("token", os.Getenv("PYCAGE_TOKEN"), "require this value in the X-Access-Token header")
	maxContexts := flags.Int("max-contexts", 32, "maximum simultaneous Python contexts")
	idleTimeout := flags.Duration("idle-timeout", 10*time.Minute, "close a context after this much inactivity")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum execution time per cell")
	memory := flags.Uint64("memory", 256<<20, "memory limit in bytes per context")
	runtimeMode := flags.String("runtime", "compiler", "Wazy runtime: compiler or interpreter")
	cacheDir := flags.String("cache-dir", defaultCacheDir(), "native compilation cache directory")
	allowNetwork := flags.Bool("network", false, "allow outbound TCP and HTTP(S) from every context")
	var binds stringList
	var cowBinds stringList
	flags.Var(&binds, "bind", "bind host directory read-write as host[=guest] (repeatable)")
	flags.Var(&cowBinds, "bind-cow", "bind host directory with an in-memory COW layer as host[=guest] (repeatable)")
	flags.Parse(argv)

	ctx := context.Background()
	filesystem, err := bindingFileSystem(binds, cowBinds)
	if err != nil {
		fatal(err)
	}
	engine, err := pycage.NewEngine(ctx, pycage.Config{
		Timeout:             *timeout,
		MemoryLimitBytes:    *memory,
		RuntimeMode:         pycage.RuntimeMode(*runtimeMode),
		CompilationCacheDir: *cacheDir,
		AllowNetwork:        *allowNetwork,
		FileSystem:          filesystem,
	})
	if err != nil {
		fatal(err)
	}
	defer engine.Close(ctx)

	server := pycage.NewServer(engine, pycage.ServerConfig{
		AccessToken: *token,
		MaxContexts: *maxContexts,
		IdleTimeout: *idleTimeout,
	})
	defer server.Close(ctx)

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal(err)
	}
	httpServer := &http.Server{Handler: server}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		graceful, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(graceful)
	}()

	fmt.Fprintf(os.Stderr, "pycage serving the E2B code-interpreter API on http://%s\n", listener.Addr())
	if *token == "" {
		fmt.Fprintln(os.Stderr, "pycage: no -token set; anyone who can reach this address can execute code")
	}
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: pycage run [options] 'python code'
       pycage serve [options]

Options:
  -timeout 5s       maximum execution time
  -memory 268435456 memory limit in bytes
  -runtime compiler  Wazy runtime: compiler or interpreter
  -cache-dir path     native cache (default: temporary directory)
  -network            allow outbound TCP and HTTP(S) from the sandbox
  -timing             print setup and execution timings
  -wheel path       install a pure-Python wheel (repeatable)
  -pip requirement  install with embedded pip (repeatable)
  -bind host[=guest] bind a host directory read-write (repeatable)
  -bind-cow host[=guest]
                     bind a host directory with memory-only writes (repeatable)
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

type binding struct {
	host  string
	guest string
	cow   bool
}

func bindingFileSystem(direct, copyOnWrite []string) (pycage.FileSystemFactory, error) {
	if len(direct) == 0 && len(copyOnWrite) == 0 {
		return nil, nil
	}
	bindings := make([]binding, 0, len(direct)+len(copyOnWrite))
	seen := make(map[string]bool, cap(bindings))
	for _, group := range []struct {
		values []string
		cow    bool
	}{{direct, false}, {copyOnWrite, true}} {
		for _, value := range group.values {
			host, guest := value, "/"
			if separator := strings.LastIndexByte(value, '='); separator >= 0 {
				host, guest = value[:separator], value[separator+1:]
			}
			if host == "" || guest == "" {
				return nil, fmt.Errorf("invalid filesystem binding %q", value)
			}
			absolute, err := filepath.Abs(host)
			if err != nil {
				return nil, fmt.Errorf("resolve bind path %q: %w", host, err)
			}
			info, err := os.Stat(absolute)
			if err != nil {
				return nil, fmt.Errorf("bind %q: %w", absolute, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("bind %q: not a directory", absolute)
			}
			guest = "/" + strings.Trim(strings.ReplaceAll(guest, "\\", "/"), "/")
			if guest == "" {
				guest = "/"
			}
			if seen[guest] {
				return nil, fmt.Errorf("duplicate guest filesystem binding %q", guest)
			}
			seen[guest] = true
			bindings = append(bindings, binding{host: absolute, guest: guest, cow: group.cow})
		}
	}
	return func() (pycage.FileSystem, error) {
		filesystem, err := pycage.DefaultFileSystem()
		if err != nil {
			return pycage.FileSystem{}, err
		}
		for _, binding := range bindings {
			mount := pycage.Bind(binding.guest, binding.host)
			if binding.cow {
				mount = pycage.CopyOnWrite(binding.guest, mount.FS, afero.NewMemMapFs())
			}
			replaced := false
			for index := range filesystem.Mounts {
				if filesystem.Mounts[index].GuestPath == binding.guest {
					filesystem.Mounts[index] = mount
					replaced = true
					break
				}
			}
			if !replaced {
				filesystem.Mounts = append(filesystem.Mounts, mount)
			}
		}
		return filesystem, nil
	}, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
