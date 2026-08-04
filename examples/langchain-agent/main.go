// Command langchain-agent gives a langchaingo agent a Python interpreter that
// runs in this process. There is no server, no container, and no API key needed
// for the sandbox itself: pycage is a library, so the tool is a struct holding a
// *pycage.Sandbox.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samyfodil/pycage"
	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms/openai"
	"github.com/tmc/langchaingo/tools"
)

// PythonSandbox is a langchaingo tool backed by one stateful pycage sandbox.
// Variables and imports survive between calls, so the agent can build up work
// across several steps the way a person would in a notebook.
type PythonSandbox struct {
	sandbox *pycage.Sandbox
}

func (p *PythonSandbox) Name() string { return "python" }

func (p *PythonSandbox) Description() string {
	return strings.TrimSpace(`
Executes Python 3 in a sandbox and returns what it printed plus the value of the
final expression. State persists between calls, so variables and imports defined
in one call are available in the next. Send raw Python, no markdown fences. End
with a bare expression when you want its value back.`)
}

func (p *PythonSandbox) Call(ctx context.Context, input string) (string, error) {
	code := strings.TrimSpace(input)
	code = strings.TrimPrefix(code, "```python")
	code = strings.TrimPrefix(code, "```")
	code = strings.TrimSuffix(code, "```")

	execution, err := p.sandbox.RunCode(ctx, code)
	if err != nil {
		// A host-level failure (timeout, trap) rather than a Python exception.
		return "", err
	}

	var reply strings.Builder
	if execution.Stdout != "" {
		reply.WriteString(execution.Stdout)
	}
	if execution.Stderr != "" {
		reply.WriteString(execution.Stderr)
	}
	// Hand the traceback back to the model rather than failing the run: a model
	// that sees its own NameError usually fixes it on the next step.
	if execution.Error != nil {
		fmt.Fprintf(&reply, "%s: %s", execution.Error.Name, execution.Error.Message)
		return reply.String(), nil
	}
	if text := execution.Text(); text != "" {
		reply.WriteString(text)
	}
	if reply.Len() == 0 {
		return "(no output)", nil
	}
	return reply.String(), nil
}

func main() {
	ctx := context.Background()

	config := pycage.DefaultConfig()
	config.Timeout = 30 * time.Second
	// Share the CLI's cache so the first run does not pay a cold native compile.
	config.CompilationCacheDir = filepath.Join(os.TempDir(), "pycage", "wazy-native")
	// Leave AllowNetwork false: this agent computes, it does not fetch.

	sandbox, err := pycage.New(ctx, config)
	if err != nil {
		log.Fatalf("create sandbox: %v", err)
	}
	defer sandbox.Close(ctx)

	tool := &PythonSandbox{sandbox: sandbox}

	// Works with no API key, so the sandbox half is verifiable on its own.
	fmt.Println("== tool check ==")
	for _, snippet := range []string{
		"import math\nradius = 3\nmath.pi * radius ** 2",
		"print('state survives'); radius * 2",
		"undefined_name",
	} {
		out, err := tool.Call(ctx, snippet)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %-42q -> %s\n", snippet, strings.TrimSpace(out))
	}

	if os.Getenv("OPENAI_API_KEY") == "" {
		fmt.Println("\nSet OPENAI_API_KEY to run the agent loop.")
		return
	}

	llm, err := openai.New()
	if err != nil {
		log.Fatalf("create llm: %v", err)
	}
	executor := agents.NewExecutor(
		agents.NewOneShotAgent(llm, []tools.Tool{tool}, agents.WithMaxIterations(5)),
	)

	question := "A 12-sided die is rolled 4 times. What is the probability that the sum is exactly 20? Compute it exactly and give the fraction."
	if len(os.Args) > 1 {
		question = strings.Join(os.Args[1:], " ")
	}

	fmt.Printf("\n== agent ==\nQ: %s\n", question)
	answer, err := chains.Run(ctx, executor, question)
	if err != nil {
		log.Fatalf("run agent: %v", err)
	}
	fmt.Printf("A: %s\n", answer)
}
