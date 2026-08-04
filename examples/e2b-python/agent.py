"""Drive a local `pycage serve` with E2B's own Python SDK.

Start the server first:

    pycage serve                      # listens on 127.0.0.1:49999

then run this file. Nothing here talks to e2b.dev.
"""

import os

from e2b_code_interpreter import Sandbox
from e2b.connection_config import ConnectionConfig
from packaging.version import Version

PYCAGE_URL = os.getenv("PYCAGE_URL", "http://127.0.0.1:49999")


def connect(url: str = PYCAGE_URL, token: str | None = None) -> Sandbox:
    """Point the E2B SDK at a pycage server.

    `Sandbox.create()` is deliberately not used: it calls E2B's control plane to
    allocate a cloud VM, which a local server has no part in. pycage implements
    the *data* plane -- /execute and the /contexts lifecycle -- so the sandbox is
    constructed directly and `sandbox_url` sends every request to pycage.
    """
    return Sandbox(
        sandbox_id="pycage-local",
        sandbox_domain=None,
        envd_version=Version("0.2.0"),
        envd_access_token=token or os.getenv("PYCAGE_TOKEN"),
        traffic_access_token=None,
        connection_config=ConnectionConfig(
            api_key="pycage-local",  # unused; the SDK insists on a value
            sandbox_url=url,
            debug=True,
        ),
    )


def show(label: str, execution) -> None:
    print(f"\n--- {label} ---")
    for line in execution.logs.stdout:
        print("stdout:", line, end="")
    for line in execution.logs.stderr:
        print("stderr:", line, end="")
    if execution.error:
        print(f"error : {execution.error.name}: {execution.error.value}")
    if execution.text is not None:
        print("result:", execution.text)


def main() -> None:
    sandbox = connect()

    show("compute", sandbox.run_code("import math\nmath.factorial(20)"))

    # State persists between calls, so an agent can build up work in steps.
    sandbox.run_code("readings = [3, 1, 4, 1, 5, 9, 2, 6]")
    show(
        "stateful follow-up",
        sandbox.run_code(
            "from fractions import Fraction\n"
            "mean = Fraction(sum(readings), len(readings))\n"
            "print(f'n={len(readings)} mean={float(mean):.3f}')\n"
            "mean"
        ),
    )

    # A Python exception is data, not a transport failure: the sandbox survives.
    show("recoverable error", sandbox.run_code("1 / 0"))
    show("still alive", sandbox.run_code("sum(readings)"))

    # Separate contexts get separate globals and separate filesystems.
    first = sandbox.create_code_context()
    second = sandbox.create_code_context()
    sandbox.run_code("secret = 'context one only'", context=first)
    show(
        "isolation",
        sandbox.run_code("globals().get('secret', 'not visible here')", context=second),
    )


if __name__ == "__main__":
    main()
