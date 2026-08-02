"""Stateful CPython guest exposed through the Component Model."""

import ast
import contextlib
import encodings.cp437
import io
import importlib.abc
import importlib.metadata._adapters
import importlib.metadata._collections
import importlib.metadata._functools
import importlib.metadata._itertools
import importlib.metadata._meta
import importlib.metadata._text
import importlib.util
import json
import os
import sys
import traceback
import types

# WASI has no Unix user database. pip's vendored platformdirs imports getuid
# while selecting cache paths, so provide the conventional single-user value.
if not hasattr(os, "getuid"):
    os.getuid = lambda: 0
if not hasattr(os, "umask"):
    os.umask = lambda mask: 0o022
if not hasattr(os, "chmod"):
    os.chmod = lambda path, mode: None

# pip's vendored cachecontrol uses mmap only to avoid copying a completed
# download buffer. WASI CPython has no mmap module; bytes provide the same
# buffer protocol and are sufficient for the callback.
if "mmap" not in sys.modules:
    mmap_compat = types.ModuleType("mmap")
    mmap_compat.ACCESS_READ = 1

    def _read_file_descriptor(file_descriptor, length, access=None):
        duplicate = os.dup(file_descriptor)
        with os.fdopen(duplicate, "rb", closefd=True) as source:
            source.seek(0)
            return source.read(length if length else -1)

    mmap_compat.mmap = _read_file_descriptor
    sys.modules["mmap"] = mmap_compat

from pip._internal.cli.main import main as _pip_main
# pip loads command implementations through importlib. componentize-py resolves
# dependencies statically, so make the install command reachable at build time.
from pip._internal.commands.install import InstallCommand as _PipInstallCommand
from pip._internal.metadata import importlib as _pip_metadata_importlib
from pip._internal.resolution.legacy import resolver as _pip_legacy_resolver
from pip._internal.resolution.resolvelib import resolver as _pip_resolvelib_resolver

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
        module_path = module.__name__.replace(".", "/")
        filename = (
            f"/site-packages/{module_path}/__init__.py"
            if entry["package"]
            else f"/site-packages/{module_path}.py"
        )
        module.__file__ = filename
        if entry["package"]:
            module.__path__ = [os.path.dirname(filename)]
        exec(compile(entry["source"], filename, "exec"), module.__dict__)

    def get_resource_reader(self, fullname):
        entry = self.modules.get(fullname)
        if entry is None or not entry["package"]:
            return None
        return _WheelResourceReader(fullname)

    def get_data(self, path):
        with open(path, "rb") as resource:
            return resource.read()


class _WheelResourceReader:
    def __init__(self, fullname):
        self.directory = "/site-packages/" + fullname.replace(".", "/")

    def open_resource(self, resource):
        return open(f"{self.directory}/{resource}", "rb")

    def resource_path(self, resource):
        return f"{self.directory}/{resource}"

    def is_resource(self, name):
        return os.path.isfile(f"{self.directory}/{name}")

    def contents(self):
        try:
            return iter(os.listdir(self.directory))
        except OSError:
            return iter(())


_wheel_loader = _WheelLoader()
sys.meta_path.insert(0, _wheel_loader)

# The host exposes an isolated in-memory filesystem. Pure-Python wheels are
# unpacked here by the trusted Go side and become importable on the next cell.
if "/site-packages" not in sys.path:
    sys.path.insert(0, "/site-packages")


def _encode_pip_result(result):
    encoded = json.dumps(result, separators=(",", ":"))
    # pip's filesystem activity currently disturbs Wazy's canonical return
    # buffer on this component. Keep the normal return, and mirror it through
    # the isolated WASI filesystem so the host can recover it reliably.
    with open("/.pycage-pip-result.json", "w", encoding="utf-8") as status:
        status.write(encoded)
    return encoded


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
            if (
                isinstance(decoded, dict)
                and set(decoded) == {"pycage_pip_arguments"}
                and isinstance(decoded["pycage_pip_arguments"], list)
            ):
                return self.pip_install(
                    json.dumps(decoded["pycage_pip_arguments"])
                )
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

    def pip_install(self, arguments: str) -> str:
        stdout = io.StringIO()
        stderr = io.StringIO()
        try:
            decoded = json.loads(arguments)
            if not isinstance(decoded, list) or not all(
                isinstance(argument, str) for argument in decoded
            ):
                raise TypeError("pip arguments must be a list of strings")
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                exit_code = _pip_main(decoded)
            return _encode_pip_result(
                {
                    "exit_code": int(exit_code or 0),
                    "stdout": stdout.getvalue(),
                    "stderr": stderr.getvalue(),
                }
            )
        except BaseException as exc:
            return _encode_pip_result(
                {
                    "exit_code": 1,
                    "stdout": stdout.getvalue(),
                    "stderr": stderr.getvalue(),
                    "error": f"{type(exc).__name__}: {exc}",
                }
            )

    def reset(self) -> str:
        _globals.clear()
        _globals.update(_INITIAL_GLOBALS)
        return "{}"
