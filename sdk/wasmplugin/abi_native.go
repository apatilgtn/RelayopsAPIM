//go:build !wasip1

package wasmplugin

import (
	"log"
	"time"
)

// Native builds (tests, go vet) log to stderr and use the local clock.

// Log writes to the gateway's log at a level: 1 debug, 2 info, 3 warn, 4 error.
func Log(level int, msg string) { log.Printf("plugin log level=%d: %s", level, msg) }

// NowMS is the gateway's clock in Unix milliseconds. Natively it is a
// variable so tests can pin the clock.
var NowMS = func() int64 { return time.Now().UnixMilli() }
