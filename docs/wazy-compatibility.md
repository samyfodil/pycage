# Wazy compatibility notes

`componentize-py 0.19.1` produces a larger and more varied component graph than
Wazy's existing Rust fixtures. The pinned Wazy revision required four focused
changes for the generated CPython component:

1. Resolve each core instantiation's `with` arguments locally. Consumer module
   names may collide when one provider is regrouped more than once.
2. Give passthrough-shim sources stable, collision-free resolver keys.
3. Support passthrough re-exports of core globals in addition to functions,
   tables, and memories.
4. Account for instance exports that append aliases to the component instance
   index space.

The CPython file path also reaches
`wasi:filesystem/types.descriptor.get-flags`, so the vendored WASI filesystem
implements that method for its in-memory descriptors.

The changes are contained in:

- `vendor/github.com/samyfodil/wazy/internal/component/instance/graph.go`
- `vendor/github.com/samyfodil/wazy/internal/component/instance/shim.go`
- `vendor/github.com/samyfodil/wazy/internal/component/instance/wasi_fs.go`

They should become upstream Wazy tests and patches. Do not regenerate `vendor/`
without preserving these changes until the pinned Wazy version includes them.
