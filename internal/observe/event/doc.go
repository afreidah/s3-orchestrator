// Package event defines the CloudEvents-shaped notification types the
// orchestrator emits when objects are created, deleted, or otherwise
// mutated, plus the Publish entry point other layers call to emit
// them. The package has zero internal dependencies so any
// caller can import it without creating a cycle.
package event
