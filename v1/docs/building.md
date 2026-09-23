# Building

## Normal case

```bash
go mod tidy
go build ./...
go test ./...
```

## This repository, as committed

The `go.mod` carries a large `replace` block pointing every dependency at a
sibling checkout. That is an artifact of the environment this was built in,
not a design choice: `proxy.golang.org` is unreachable from it, and the
vanity import paths (`connectrpc.com/...`, `go.opentelemetry.io/...`) cannot
be resolved either, so `GOPROXY=direct` does not help.

The dependencies are ordinary modules at ordinary release tags, cloned from
their upstream GitHub repositories.

### To build it as committed

Clone the dependencies next to this repository:

```bash
mkdir -p ../../godeps && cd ../../godeps
git clone --branch v1.19.2  https://github.com/connectrpc/connect-go.git       connect-go
git clone --branch v0.9.0   https://github.com/connectrpc/otelconnect-go.git   otelconnect
git clone --branch v1.38.0  https://github.com/open-telemetry/opentelemetry-go.git otel
git clone --branch v0.50.0  https://github.com/golang/net.git                  net
# ... and the rest; every path in the replace block maps to one clone
```

The full list is the `replace` block itself — each line names the module and
the directory it expects.

### To build it normally

Delete the `replace` block and run `go mod tidy`. Nothing in the framework
depends on the block existing; no source file imports anything unusual. This
is the first thing to do on a machine with a working module proxy.

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
