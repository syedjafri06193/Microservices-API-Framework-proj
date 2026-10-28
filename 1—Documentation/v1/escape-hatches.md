# Escape hatches

Every framework that gets adopted can be partially abandoned. Teams adopt
tools they can walk away from incrementally, and a framework that owns
`main()` with no way out is one nobody will risk.

Each of these works with none of the rest of the framework.

## The interceptor chain

```go
svc, _ := service.New()

path, handler := userv1connect.NewUserServiceHandler(impl,
    connect.WithInterceptors(svc.ServerInterceptors()...))

// Your server, your mux, your lifecycle.
mux := http.NewServeMux()
mux.Handle(path, handler)
http.ListenAndServe(":8080", h2c.NewHandler(mux, &http2.Server{}))
```

Realistically this is what people will take. That is fine: a framework whose
best-used part is one function is still a framework that helped.

The client side is the same shape:

```go
interceptors, _ := svc.ClientInterceptors("payments")
client := paymentv1connect.NewPaymentServiceClient(httpClient, addr,
    connect.WithInterceptors(interceptors...))
```

## The shutdown sequencer

```go
seq := lifecycle.NewSequencer(logger,
    lifecycle.Step{Name: "fail readiness", Run: ...},
    lifecycle.Step{Name: "drain", Run: ..., ContinueOnError: true},
    lifecycle.Step{Name: "flush telemetry", Run: ...},
)
err := seq.Run(ctx, 30*time.Second)
```

Nothing above imports the rest of the framework. `lifecycle.Timing` will
also check the `PreStopDelay + DrainTimeout < gracePeriod` arithmetic for
you, which is the part that is easy to get wrong because the two halves live
in different files.

## The resilience primitives

```go
breakers, _ := breaker.NewGroup(breaker.DefaultConfig(), nil, onStateChange)
b := breakers.Get(breaker.Key("payments", "/payment.v1.PaymentService/Get"))

done, err := b.Allow()
if err != nil { /* open */ }
done(callErr)
```

`resilience.CountsAsFailure` and `resilience.IsRetryable` are exported too.
If you take nothing else from this project, those two functions encode the
decision that matters most.

## The health tracker

```go
h := lifecycle.NewHealth()
h.AddLocalCheck("pool", func() error { ... })
mux.Handle("/readyz", h.Handler())
```

## The raw mux

```go
svc.Mux().Handle("GET /internal/something", myHandler)
svc.AdminHandle("GET /debug/mine", myDebugHandler)
```

## Test helpers

`fwtest` is usable against any Connect handler, framework or not:

```go
srv := fwtest.NewServer(t, echov1connect.NewEchoServiceHandler(impl),
    fwtest.WithSpanRecording())
// ... make calls ...
span := srv.Recorder.Require(t, "/echo.v1.EchoService/Echo")
require.True(t, span.HasError())
```
