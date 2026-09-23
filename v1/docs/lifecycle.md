# Startup, health and shutdown

## Startup

```
1. Load config from the environment
2. Validate exhaustively — exit non-zero naming the exact field
3. Initialize telemetry (so startup failures are traced)
4. Construct dependencies — connect eagerly, fail if unreachable
5. Build the interceptor chain
6. Start the admin server — /healthz passes, /readyz does NOT yet
7. Run startup hooks — cache warm, migration check, leader election
8. Start the main server
9. Flip /readyz to ready
10. Block
```

Two orderings matter. The admin server starts **before** the main one so a
startup probe can observe progress. `/readyz` flips **after** the main
listener is accepting — flipping it earlier sends traffic to a connection
refused.

A service that starts successfully with a misconfigured database and fails
on the first request is worse than one that refuses to start. Validation
errors name the variable, the value and the constraint:

```
config: ENVIRONMENT must be one of [dev staging prod], got "production"
```

and every problem is reported at once, so an operator fixing a broken
deployment gets the whole list rather than discovering the next missing
variable on each restart.

## Health: three different questions

| Endpoint | Question | Checks dependencies? |
|---|---|---|
| `/healthz` (liveness) | Is this process wedged and in need of a restart? | **No. Never.** |
| `/readyz` (readiness) | Should this instance receive traffic right now? | **Almost never.** |
| `/startupz` (startup) | Has initialization finished? | Only its own init |

### The cascading-failure trap

If `/readyz` checks the database, a ten-second database blip marks *every*
replica unready simultaneously. The load balancer removes all of them, and a
partial degradation — where the service could have served cached reads and
returned clean errors for the rest — becomes a total outage.

Worse, it cannot recover: there are no healthy endpoints left to route
recovery traffic to.

**Readiness answers "can *this instance* serve?"** Not "is the whole system
healthy?". Legitimate signals: initialization finished, not draining, local
resources available *for this instance* — a connection pool this process has
exhausted, not a database everybody shares.

`Health.AddLocalCheck` exists for those, and the name is deliberately
awkward so that adding a database ping looks wrong at the call site.

Liveness is for genuinely unrecoverable process-local state. A dependency
being down is not a reason to ask for a restart: restarting will not fix it,
and a crash loop across every replica turns a dependency blip into an outage
of your own.

## Shutdown

Two non-obvious facts drive the sequence.

**SIGTERM arrives before traffic stops.** Kubernetes sends SIGTERM and
removes the pod's endpoint concurrently. Endpoint removal propagates through
kube-proxy and every ingress and sidecar asynchronously, over seconds. A
process that exits immediately drops in-flight requests *and* receives new
ones after it has decided to die.

**Telemetry must flush after the server stops.** Flushing first loses every
span from the requests still draining — exactly the spans that explain a
shutdown-time incident.

```
1. Fail readiness                    load balancers start removing us
2. Wait PreStopDelay                 for that to propagate
3. Drain the main server             in-flight requests finish
4. Close dependencies                nothing is producing new work now
5. Drain the admin server            probes and scrapes worked until now
6. Flush telemetry                   last, with context.WithoutCancel
```

Step 2 is not a hack and not optional. Endpoint removal is eventually
consistent and there is no signal that says "every load balancer has stopped
sending you traffic", so you wait for as long as your platform takes.

Step 6 uses `context.WithoutCancel`. Shutdown is usually triggered by a
cancelled context, and using it directly would cancel the flush immediately
— losing the spans that explain why you shut down.

### The arithmetic must hold

```
PreStopDelay + DrainTimeout < terminationGracePeriodSeconds
```

If it does not, the platform sends SIGKILL mid-drain and all of the above is
wasted. The two halves live in different files — this framework's config and
a Kubernetes manifest — so it is easy to get wrong. Supply the grace period
from the downward API:

```yaml
env:
  - name: TERMINATION_GRACE_PERIOD
    value: "30s"
```

and the framework checks it at startup, where both numbers are visible.
