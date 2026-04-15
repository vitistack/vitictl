package talos

import (
	"net"
	"strconv"
	"time"
)

// APIPort is the TCP port the Talos API (apid) listens on by default.
// All `talosctl …` commands connect to endpoints on this port.
const APIPort = 50000

// DefaultProbeTimeout is the per-address timeout for a reachability
// probe. Short enough to fail fast on unroutable addresses, long enough
// to avoid spurious false negatives on slow networks.
const DefaultProbeTimeout = 2 * time.Second

// ProbeResult is the outcome of one reachability check.
type ProbeResult struct {
	Address string
	OK      bool
	Err     error
}

// ProbeReachable probes each address on the given TCP port in parallel
// (one DialTimeout per address) and returns the full result list in the
// input order, plus the filtered list of reachable addresses. Useful
// for telling "where are we trying to connect" apart from "which of
// these is actually routable from this machine".
func ProbeReachable(addrs []string, port int, timeout time.Duration) ([]ProbeResult, []string) {
	if len(addrs) == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	results := make([]ProbeResult, len(addrs))
	done := make(chan int, len(addrs))
	for i, a := range addrs {
		go func(i int, a string) {
			target := net.JoinHostPort(a, strconv.Itoa(port))
			conn, err := net.DialTimeout("tcp", target, timeout)
			if err != nil {
				results[i] = ProbeResult{Address: a, OK: false, Err: err}
				done <- i
				return
			}
			_ = conn.Close()
			results[i] = ProbeResult{Address: a, OK: true}
			done <- i
		}(i, a)
	}
	for i := 0; i < len(addrs); i++ {
		<-done
	}

	reachable := make([]string, 0, len(addrs))
	for _, r := range results {
		if r.OK {
			reachable = append(reachable, r.Address)
		}
	}
	return results, reachable
}
