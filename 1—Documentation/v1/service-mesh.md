# When to turn this off

If your services run on Kubernetes with Istio or Linkerd, the sidecar
already does retries, timeouts, circuit breaking, mTLS and traffic-level
metrics. Duplicating that in-process is not merely redundant — it is
actively dangerous.

## The hazards

| Hazard | Mechanism |
|---|---|
| **Retry amplification** | App retries 3×, mesh retries 3× → 9× load per logical call, per hop. Across three hops, 27×. A minor blip becomes a cascading failure. |
| **Deadline confusion** | Both layers enforce timeouts with different values; you get inconsistent, hard-to-debug cancellation. |
| **Double-counted metrics** | Mesh RPS and app RPS disagree and nobody knows which to trust. |
| **Conflicting breakers** | The mesh ejects an endpoint while the app breaker is closed, or the reverse. |

The amplification one is the worst because nothing looks wrong at any single
layer. Each is doing something reasonable; the product is not.

## What the framework does about it

It detects a sidecar at startup — Istio and Linkerd environment variables,
then a loopback probe of the proxy admin ports — and if retries are also
enabled in-process it logs:

```
retries are enabled in-process AND a service mesh sidecar was detected;
this may amplify load during a partial outage
```

It does **not** disable anything. Detection is heuristic, and a false
positive that silently removed a service's resilience would be much worse
than one that printed a warning. The operator gets the line and one
environment variable.

## What to turn off, and what to keep

With a mesh, set:

```
RETRY_ENABLED=false      # let the mesh own retries
BREAKER_ENABLED=false    # and outlier ejection
```

Keep everything else. The parts worth having in-process are the ones a
sidecar structurally cannot do:

- **Semantic failures the mesh cannot see.** A `200 OK` containing
  `{"error": ...}`, a slow-but-successful response, a partial result. The
  mesh sees a successful HTTP response; your handler sees the truth.
- **Per-method policy.** The mesh sees paths; the framework sees procedures
  and proto annotations, which is what makes
  "retry only what is declared idempotent" possible at all.
- **Deadline budget arithmetic.** No mesh does this. It is the single most
  valuable thing here under load.
- **Load shedding on queue latency.** The mesh cannot see your queue.
- **Everything observability.** Spans, trace-correlated logs, the
  cardinality guard.
- **The lifecycle.** Readiness semantics and the shutdown sequence are
  process-local and the mesh has no opinion about them.

## Without a mesh

VMs, ECS, Cloud Run, Nomad, local development. Leave the defaults on: this
is the deployment the resilience layer was written for.
