"""Stateful CPython guest exposed through the Component Model."""

import ast
import contextlib
import io
import importlib.abc
import importlib.util
import json
import sys
import traceback

from wit_world import exports


_INITIAL_GLOBALS = {
    "__name__": "__pycage__",
    "__builtins__": __builtins__,
}
_globals = dict(_INITIAL_GLOBALS)


class _WheelLoader(importlib.abc.MetaPathFinder, importlib.abc.Loader):
    def __init__(self):
        self.modules = {}

    def find_spec(self, fullname, path=None, target=None):
        entry = self.modules.get(fullname)
        if entry is None:
            return None
        return importlib.util.spec_from_loader(
            fullname, self, is_package=entry["package"]
        )

    def create_module(self, spec):
        return None

    def exec_module(self, module):
        entry = self.modules[module.__name__]
        filename = f"<wheel:{module.__name__}>"
        module.__file__ = filename
        if entry["package"]:
            module.__path__ = []
        exec(compile(entry["source"], filename, "exec"), module.__dict__)


_wheel_loader = _WheelLoader()
sys.meta_path.insert(0, _wheel_loader)

# The host exposes an isolated in-memory filesystem. Pure-Python wheels are
# unpacked here by the trusted Go side and become importable on the next cell.
if "/site-packages" not in sys.path:
    sys.path.insert(0, "/site-packages")


def _display(value):
    if value is None:
        return []

    outputs = []
    rich_reprs = (
        ("_repr_html_", "html"),
        ("_repr_svg_", "svg"),
        ("_repr_json_", "json"),
    )
    for method_name, kind in rich_reprs:
        method = getattr(value, method_name, None)
        if method is not None:
            try:
                rendered = method()
                if rendered is not None:
                    if kind == "json" and not isinstance(rendered, str):
                        rendered = json.dumps(rendered)
                    outputs.append({"type": kind, "data": rendered})
                    return outputs
            except Exception:
                pass

    outputs.append({"type": "text", "data": repr(value)})
    return outputs


def _execute(code):
    tree = ast.parse(code, filename="<pycage>", mode="exec")
    last_expression = None
    if tree.body and isinstance(tree.body[-1], ast.Expr):
        last_expression = ast.Expression(tree.body.pop().value)

    if tree.body:
        exec(compile(tree, "<pycage>", "exec"), _globals, _globals)

    if last_expression is not None:
        return eval(compile(last_expression, "<pycage>", "eval"), _globals, _globals)
    return None


class CodeInterpreter(exports.CodeInterpreter):
    def run_code(self, code: str) -> str:
        stdout = io.StringIO()
        stderr = io.StringIO()
        result = {
            "outputs": [],
            "stdout": "",
            "stderr": "",
            "error": None,
        }

        try:
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                value = _execute(code)
            result["outputs"] = _display(value)
        except BaseException as exc:
            result["error"] = {
                "name": type(exc).__name__,
                "message": str(exc),
                "traceback": traceback.format_exc(),
            }

        result["stdout"] = stdout.getvalue()
        result["stderr"] = stderr.getvalue()
        return json.dumps(result, separators=(",", ":"))

    def install_modules(self, modules: str) -> str:
        try:
            decoded = json.loads(modules)
            for name, entry in decoded.items():
                if not isinstance(name, str) or not isinstance(entry, dict):
                    raise TypeError("invalid module entry")
                source = entry.get("source")
                package = entry.get("package")
                if not isinstance(source, str) or not isinstance(package, bool):
                    raise TypeError(f"invalid module {name!r}")
            _wheel_loader.modules.update(decoded)
            return "{}"
        except BaseException as exc:
            return json.dumps({"error": f"{type(exc).__name__}: {exc}"})

    def reset(self) -> str:
        _globals.clear()
        _globals.update(_INITIAL_GLOBALS)
        return "{}"
