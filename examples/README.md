# Examples

Three ways to put pycage behind an agent.

| Example | Talks to pycage via | Needs a server? |
| --- | --- | --- |
| [`langchain-agent`](langchain-agent) | the Go package, in-process | no |
| [`e2b-python`](e2b-python) | E2B's Python SDK over HTTP | yes |
| [`e2b-typescript`](e2b-typescript) | E2B's JS/TS SDK over HTTP | yes |

## langchain-agent

A [langchaingo](https://github.com/tmc/langchaingo) tool whose `Call` runs the
model's code in a `*pycage.Sandbox` held in the same process. No server, no
container, no socket. The sandbox is stateful, so the agent can define a
variable in one step and use it in the next.

```console
cd examples/langchain-agent
go run .                      # exercises the tool with no API key
OPENAI_API_KEY=sk-... go run . "how many primes below 10 million?"
```

Without a key it still runs the tool directly, so you can see the sandbox work
before wiring up an LLM. The interesting part is that a Python traceback is
returned to the model as a normal observation rather than failing the run — a
model that sees its own `NameError` usually fixes it on the next step.

## Server mode and the E2B SDKs

`pycage serve` speaks E2B's code-interpreter HTTP API, so E2B's own SDKs drive it
unmodified once pointed at the local address:

```console
pycage serve                  # 127.0.0.1:49999
```

```console
cd examples/e2b-python && pip install -r requirements.txt && python agent.py
```

```console
cd examples/e2b-typescript && npm install && npm start
```

Both print the same five checks: a computation, a stateful follow-up, a
recoverable `ZeroDivisionError`, proof the context survived it, and proof that
two contexts cannot see each other's globals.

### What "E2B-compatible" means here

pycage implements E2B's **data plane** — `POST /execute` and the `/contexts`
lifecycle — which is the part that runs code. It does not implement E2B's
**control plane**, the cloud API that allocates VMs.

So `Sandbox.create()` is not used in either example: that call reaches out to
E2B to provision a machine, and a local process has no part in it. The sandbox
object is constructed directly instead, with `sandbox_url` / `sandboxUrl`
pointing every request at pycage. Both examples wrap that in a `connect()`
helper — it is the only thing that differs from ordinary E2B code.

Everything past `connect()` is stock SDK: `run_code`, `logs.stdout`,
`.results`, `.error`, `create_code_context`.

### Authentication

`pycage serve` binds to loopback and takes no token by default. Expose it beyond
localhost only with `-token` set, which is then required in `X-Access-Token`:

```console
pycage serve -addr 0.0.0.0:49999 -token "$(openssl rand -hex 32)"
```

Both examples read that token from `PYCAGE_TOKEN`.
