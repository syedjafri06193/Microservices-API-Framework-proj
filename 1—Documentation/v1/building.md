# Building

## Normal case

```bash
go mod tidy
go build ./...
go test ./...
```

## Offline builds

There used to be a big `replace` block in `go.mod` for an airgapped box.
Its gone now so normal proxy builds work. If you still need offline, clone
deps under `../../godeps` and add your own replaces — see an older commit
for the full list.

## Regenerating the protobuf code

`gen/` is committed so that the repository builds without a protobuf
toolchain. To regenerate:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go
go install connectrpc.com/connect/cmd/protoc-gen-connect-go

protoc --proto_path=proto \
  --go_out=gen --go_opt=module=github.com/syedjafri06193/microservices-api-framework/gen \
  --connect-go_out=gen --connect-go_opt=module=github.com/syedjafri06193/microservices-api-framework/gen \
  proto/echo/v1/echo.proto
```

The design document specifies `buf` (§11.2), which is the right tool for a
real project — it handles dependency management, linting and breaking-change
detection, none of which bare `protoc` does. `protoc` is used here because
it installs from the distribution's package manager and `buf` does not, in
this environment.

A `buf.yaml` and `buf.gen.yaml` equivalent to the command above:

```yaml
# buf.yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

```yaml
# buf.gen.yaml
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: github.com/syedjafri06193/microservices-api-framework/gen
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
```
