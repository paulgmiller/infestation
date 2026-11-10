package smoke

import "io"

// Options adds optional host Tailscale latency measurements to the pod checks.
type Options struct {
	TailnetLatency bool
	Log            io.Writer
}
