// -------------------------------------------------------------------------------
// Ops Test Doubles - Declared Buckets
//
// Author: Alex Freidah
//
// A declared bucket set for tests that stand up the operations layer and need
// its passes to reach every bucket, as a deployment does when no bucket sets a
// stored form of its own.
// -------------------------------------------------------------------------------

package opstest

import (
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
)

// EveryLayer returns a declared set holding names, whose new objects all carry
// every layer, so a pass adding one is scoped to every bucket.
func EveryLayer(names ...string) *provisioning.Declared {
	buckets := make([]provisioning.Bucket, 0, len(names))
	for _, name := range names {
		buckets = append(buckets, provisioning.Bucket{Name: name, Source: provisioning.SourceStore})
	}
	d := provisioning.NewDeclared()
	d.Store(buckets)
	d.UseDeployment(config.NewLive(&config.Config{
		Compression: config.CompressionConfig{Enabled: true},
		Integrity:   config.IntegrityConfig{Enabled: true},
	}), true)
	return d
}
