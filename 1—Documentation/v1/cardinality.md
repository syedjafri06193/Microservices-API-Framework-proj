# Cardinality

A Prometheus time series exists for every unique label combination. A `path`
label containing `/users/12345` produces one series per user. At a million
users that is a million series, and your metrics backend dies — usually
during the incident you built the metrics for.

## Making it structurally impossible

Documentation does not prevent this; a closed type does.

```go
type MethodLabel struct{ value string }

func FromProcedure(procedure string) MethodLabel { return MethodLabel{value: procedure} }
```

The field is unexported, so the only way to obtain a `MethodLabel` is
`FromProcedure`, which takes a Connect procedure — always the static
template `/package.Service/Method` generated from the proto, never anything
a request can influence.

A user *cannot* pass an arbitrary string, even by accident. This is the kind
of guarantee a framework can offer and a library cannot, and it is one of
the better arguments for this project existing.

`PeerLabel` is the same idea for outbound calls: it comes from configuration,
so a hostile caller cannot mint series by varying a header.

## Safe label sets

| Safe | Never |
|---|---|
| RPC method (static, from the proto) | Raw URL path |
| Status code | User ID, tenant ID, request ID |
| Service name | Error message text |
| Environment, region | Timestamps |
| Peer name (from configuration) | Arbitrary headers |

## The guard

`telemetry.CardinalityGuard` counts distinct label combinations per metric
and warns above a threshold:

```
metric cardinality is high  metric=rpc.server.duration series=1000
  hint="check for an unbounded label; see docs/cardinality.md"
```

Current counts are on the admin port at `/debug/cardinality`. Catching this
in staging is worth a great deal; catching it in production means someone is
already paging.

The guard is bounded itself. Past a hard cap it stops tracking a runaway
metric, because a guard that OOMs the process it is protecting has made
things worse. A metric it has given up on reports `-1`.
