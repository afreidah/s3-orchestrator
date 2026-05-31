// Compile-time pins for #986: each narrow role interface declared in
// internal/proxy/object/consumer_interfaces.go must be satisfied
// implicitly by *writepath.Coordinator, so future test mocks and
// consumers can depend on whichever role matches their actual call
// surface without dragging the full ObjectCoordinator surface along.
//
// Lives in the proxy package because that's the composition root that
// already imports both writepath and object — adding these assertions
// inside object/ would force an import cycle.

package proxy

import (
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
)

var (
	_ object.WriteRouter   = (*writepath.Coordinator)(nil)
	_ object.PendingWriter = (*writepath.Coordinator)(nil)
	_ object.CleanupWriter = (*writepath.Coordinator)(nil)
)
