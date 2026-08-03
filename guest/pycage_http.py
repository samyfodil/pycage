"""HTTPS client backed by the host's standard WASI HTTP implementation."""

from dataclasses import dataclass
from io import BytesIO
from urllib.parse import urlsplit

from componentize_py_types import Err, Ok
from wit_world.imports import outgoing_handler
from wit_world.imports.streams import StreamError_Closed
from wit_world.imports.types import (
    Fields,
    IncomingBody,
    Method_Delete,
    Method_Get,
    Method_Head,
    Method_Options,
    Method_Other,
    Method_Patch,
    Method_Post,
    Method_Put,
    OutgoingBody,
    OutgoingRequest,
    Scheme_Http,
    Scheme_Https,
)


@dataclass(frozen=True)
class Response:
    status_code: int
    content: bytes
    url: str

    @property
    def text(self) -> str:
        return self.content.decode("utf-8", errors="replace")


_METHODS = {
    "GET": Method_Get,
    "HEAD": Method_Head,
    "POST": Method_Post,
    "PUT": Method_Put,
    "DELETE": Method_Delete,
    "OPTIONS": Method_Options,
    "PATCH": Method_Patch,
}


def request(method: str, url: str, headers=None, body=None) -> Response:
    parsed = urlsplit(url)
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        raise ValueError("URL must use http or https and include an authority")

    request_headers = Fields()
    for name, value in (headers or {}).items():
        values = value if isinstance(value, (list, tuple)) else (value,)
        request_headers.set(
            str(name).lower(), [str(item).encode("utf-8") for item in values]
        )

    outgoing = OutgoingRequest(request_headers)
    method_name = method.upper()
    method_type = _METHODS.get(method_name)
    outgoing.set_method(
        method_type() if method_type is not None else Method_Other(method_name)
    )
    outgoing.set_scheme(
        Scheme_Https() if parsed.scheme == "https" else Scheme_Http()
    )
    outgoing.set_authority(parsed.netloc)
    outgoing.set_path_with_query(
        (parsed.path or "/") + (("?" + parsed.query) if parsed.query else "")
    )

    if body is not None:
        payload = body.encode("utf-8") if isinstance(body, str) else bytes(body)
        outgoing_body = outgoing.body()
        output = outgoing_body.write()
        try:
            for offset in range(0, len(payload), 4096):
                output.blocking_write_and_flush(payload[offset : offset + 4096])
        finally:
            output.__exit__(None, None, None)
        OutgoingBody.finish(outgoing_body, None)

    future = outgoing_handler.handle(outgoing, None)
    resolved = future.get()
    if resolved is None:
        pollable = future.subscribe()
        try:
            pollable.block()
        finally:
            pollable.__exit__(None, None, None)
        resolved = future.get()
    if not isinstance(resolved, Ok) or not isinstance(resolved.value, Ok):
        error = resolved.value if isinstance(resolved, (Ok, Err)) else resolved
        if isinstance(error, Err):
            error = error.value
        raise OSError(f"WASI HTTP request failed: {error!r}")

    incoming = resolved.value.value
    status = incoming.status()
    incoming_body = incoming.consume()
    stream = incoming_body.stream()
    chunks = []
    try:
        while True:
            try:
                chunks.append(stream.blocking_read(64 * 1024))
            except Err as error:
                if isinstance(error.value, StreamError_Closed):
                    break
                raise
    finally:
        stream.__exit__(None, None, None)
    IncomingBody.finish(incoming_body)
    incoming.__exit__(None, None, None)
    future.__exit__(None, None, None)
    return Response(status, b"".join(chunks), url)


def get(url: str, headers=None) -> Response:
    return request("GET", url, headers=headers)


def install_requests_adapter() -> bool:
    """Route requests through WASI HTTP when the package is installed."""
    try:
        import requests
        import requests.sessions
        from requests.adapters import BaseAdapter
        from requests.models import Response as RequestsResponse
        from requests.structures import CaseInsensitiveDict
    except ImportError:
        return False

    if getattr(requests.sessions, "_pycage_wasi_http", False):
        return True

    class WASIHTTPAdapter(BaseAdapter):
        def send(
            self,
            prepared_request,
            stream=False,
            timeout=None,
            verify=True,
            cert=None,
            proxies=None,
        ):
            try:
                request_headers = dict(prepared_request.headers)
                request_headers["Accept-Encoding"] = "identity"
                received = request(
                    prepared_request.method,
                    prepared_request.url,
                    headers=request_headers,
                    body=prepared_request.body,
                )
            except Exception as error:
                raise requests.exceptions.ConnectionError(
                    str(error), request=prepared_request
                ) from error

            response = RequestsResponse()
            response.status_code = received.status_code
            response.url = received.url
            response.request = prepared_request
            response.connection = self
            response.headers = CaseInsensitiveDict()
            response.raw = BytesIO(received.content)
            response._content = received.content
            response._content_consumed = True
            return response

        def close(self):
            pass

    requests.sessions.HTTPAdapter = WASIHTTPAdapter
    requests.adapters.HTTPAdapter = WASIHTTPAdapter
    requests.sessions._pycage_wasi_http = True
    return True
