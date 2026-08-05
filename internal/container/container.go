// Package container is the design-§5 container adapter: it owns every
// docker and devcontainer invocation, translating their output into
// domain observations. No higher layer parses docker output or renders
// docker argv.
package container

import (
	"time"

	"github.com/gambtho/projectmux/internal/controller"
)

// DefaultTimeout bounds probes and discovery. Startup gets the
// configuration's start_timeout instead (spec §3).
const DefaultTimeout = 5 * time.Second

// The executables to invoke; tests substitute scripts.
var (
	dockerBinary       = "docker"
	devcontainerBinary = "devcontainer"
)

// Adapter implements the controller's container observer and actuator.
// Timeout bounds probes and discovery (zero means DefaultTimeout);
// StartTimeout overrides the per-call start_timeout for tests only.
type Adapter struct {
	Timeout      time.Duration
	StartTimeout time.Duration
}

var (
	_ controller.ContainerObserver = (*Adapter)(nil)
	_ controller.ContainerActuator = (*Adapter)(nil)
)

func (a *Adapter) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return DefaultTimeout
}
