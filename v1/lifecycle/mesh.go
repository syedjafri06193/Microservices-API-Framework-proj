package lifecycle

import (
	"net"
	"os"
	"time"
)

// DetectMesh reports whether a service mesh sidecar appears to be present.
//
// This exists for one startup log line, and that line is worth more than
// any amount of documentation:
//
//	retries are enabled in-process AND a mesh sidecar was detected;
//	this may amplify load
//
// The hazard is real and badly understood. If the app retries three times
// and the mesh retries three times, one logical call becomes nine requests
// per hop — across three hops, 27x. A minor blip becomes a cascading
// failure, and the people debugging it see a traffic spike with no obvious
// source, because no single layer is doing anything unreasonable.
//
// Detection is best-effort and deliberately conservative. A false positive
// prints a warning about a real hazard; a false negative simply prints
// nothing. Neither changes behaviour — the framework warns, it does not
// silently disable resilience, because guessing wrong about that would be
// worse than either.
func DetectMesh() (string, bool) {
	// Istio and Linkerd both set well-known environment variables in the
	// injected pod spec.
	for _, env := range []struct{ key, mesh string }{
		{"ISTIO_META_WORKLOAD_NAME", "istio"},
		{"ISTIO_META_MESH_ID", "istio"},
		{"LINKERD2_PROXY_DESTINATION_SVC_ADDR", "linkerd"},
		{"LINKERD2_PROXY_LOG_LEVEL", "linkerd"},
	} {
		if os.Getenv(env.key) != "" {
			return env.mesh, true
		}
	}

	// Failing that, look for a sidecar admin port on loopback. Short
	// timeout: this runs during startup, and a slow probe delays the moment
	// the service becomes ready.
	for _, probe := range []struct{ addr, mesh string }{
		{"127.0.0.1:15000", "istio"},  // Envoy admin
		{"127.0.0.1:4191", "linkerd"}, // linkerd-proxy admin
	} {
		conn, err := net.DialTimeout("tcp", probe.addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return probe.mesh, true
		}
	}

	return "", false
}
