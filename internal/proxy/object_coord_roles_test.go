// Compile-time pins for the narrow role interfaces declared in
// internal/proxy/object/consumer_interfaces.go. Each must be satisfied
// implicitly by the concrete producer so future test mocks and
// consumers can depend on whichever role matches their actual call
// surface without dragging the full composite surface along.
//
// Lives in the proxy package because that's the composition root that
// already imports both writepath/infra and object — adding these
// assertions inside object/ would force an import cycle.

package proxy

import (
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
)

// ObjectCoordinator role split: writepath.Coordinator satisfies each
// narrow write-side role implicitly.
var (
	_ object.WriteRouter   = (*writepath.Coordinator)(nil)
	_ object.PendingWriter = (*writepath.Coordinator)(nil)
	_ object.CleanupWriter = (*writepath.Coordinator)(nil)
)

// ObjectRuntime role split: *infra.BackendRuntime satisfies the read and write
// Manager-side roles plus the composite.
var (
	_ object.ObjectWriteRuntime = (*infra.BackendRuntime)(nil)
	_ object.ObjectReadRuntime  = (*infra.BackendRuntime)(nil)
	_ object.ObjectRuntime      = (*infra.BackendRuntime)(nil)
)
