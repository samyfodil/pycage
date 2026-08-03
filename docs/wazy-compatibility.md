# Wazy compatibility notes

`componentize-py 0.19.1` produces a larger and more varied component graph than
Wazy's original Rust fixtures. The Wazy revision pinned by pycage now includes
the required upstream support:

1. Resolve each core instantiation's `with` arguments locally. Consumer module
   names may collide when one provider is regrouped more than once.
2. Give passthrough-shim sources stable, collision-free resolver keys.
3. Support passthrough re-exports of core globals in addition to functions,
   tables, and memories.
4. Account for instance exports that append aliases to the component instance
   index space.

It also includes real `FSConfig` mounts for Component Model WASI 0.2 plus the
`descriptor.get-flags`, `descriptor.sync`, and `descriptor.sync-data` methods
CPython reaches. Pycage consumes that revision directly through `go.mod` and
does not vendor or locally patch it.

The componentized CPython socket adapter currently passes an unissued network
resource when binding an IPv6 socket. Because urllib3 probes IPv6 with a bind
at import time, pycage reports `socket.has_ipv6` as false until that adapter path
is fixed. This prevents an unsupported capability from causing imports to trap;
it does not enable guest socket binds.

HTTPS uses Wazy's stock `wasi:http/outgoing-handler@0.2.0` implementation and
Go's TLS stack because componentize-py's CPython does not ship `_ssl`. The
current Wazy HTTP surface is sufficient for request method, URL, headers, body,
response status, and response body. A complete requests response still needs
these upstream WASI HTTP methods:

- `wasi:http/types@0.2.0#[method]incoming-response.headers`
- `wasi:http/types@0.2.0#[method]fields.entries`

Pycage currently returns an empty `requests.Response.headers` and requests
identity encoding so it never guesses whether an opaque response body is
compressed.

One upstream gap remains:

- `wasi:filesystem/types@0.2.0#[method]descriptor.set-times-at` is not
  implemented. CPython's `os.utime` therefore traps. Pip can reach it when a
  cross-mount rename falls back to a metadata-preserving copy.

Pycage avoids that pip path by installing into `/pycage-install` on the root
mount and finalizing validated package files into `/site-packages` through the
host-side Afero mounts. General guest `os.utime` remains unsupported until Wazy
implements `set-times-at`.
