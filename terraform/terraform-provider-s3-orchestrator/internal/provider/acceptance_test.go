// -------------------------------------------------------------------------------
// Acceptance Test Harness
//
// Author: Alex Freidah
//
// Stands a real orchestrator up in a container and points the provider at it,
// so every acceptance test drives genuine terraform plan, apply, refresh and
// import against the same admin API an operator would.
//
// The orchestrator arrives as an image rather than as a package this module
// imports. That keeps the provider a standalone Go module, which is what it has
// to be to be published on its own, and it tests the artifact that actually
// ships. `make provider-test` builds the image first; the Dockerfile needs
// BuildKit, which the Docker client library this uses does not speak.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// envImage names the orchestrator image to run, and defaultImage is what
// `make provider-test` builds. CI builds the image once for the whole pipeline
// and points the variable at that one instead.
const (
	envImage     = "S3O_TEST_IMAGE"
	defaultImage = "s3-orchestrator:acceptance"
)

// The root keypair the container is configured with, and which the provider
// signs its admin requests as. It is a fixture rather than a secret: the
// container it reaches lives for the length of one test binary.
const (
	testAccessKeyID     = "AKIAACCEPTANCETEST00"
	testSecretAccessKey = "acceptance-test-secret"
)

// testBucket is the one bucket the container serves. Grant tests name it
// because a grant over a bucket the deployment does not have is refused.
const testBucket = "photos"

// orchestratorPort is what the image exposes and the configuration listens on.
const orchestratorPort = "9000/tcp"

// startupTimeout bounds the wait for a ready orchestrator.
const startupTimeout = time.Minute

// testConfig is the smallest configuration the orchestrator will boot on.
//
// The backend points at a closed port on purpose. Nothing here moves an
// object, and a reachable backend would mean running MinIO alongside for no
// coverage: what is under test is the control plane, which never calls one.
const testConfig = `
server:
  listen_addr: ":9000"

auth:
  root:
    access_key_id: ` + testAccessKeyID + `
    secret_access_key: ` + testSecretAccessKey + `

database:
  driver: sqlite
  path: /tmp/s3o.db

backends:
  - name: unreachable
    endpoint: http://127.0.0.1:1
    region: us-east-1
    bucket: acceptance
    access_key_id: backend-key
    secret_access_key: backend-secret
    force_path_style: true

buckets:
  - name: ` + testBucket + `
    credentials:
      - access_key_id: AKIAACCEPTANCEDATA00
        secret_access_key: acceptance-data-secret
`

// -------------------------------------------------------------------------
// FIXTURE
// -------------------------------------------------------------------------

// testAccAddress is the running orchestrator's admin address, set by TestMain
// and read by the helpers that talk to it directly.
var testAccAddress string

// testAccProtoV6ProviderFactories is how the test harness obtains the provider:
// in process, rather than by launching the built binary. Terraform still drives
// it over its plugin protocol, so the code path is the real one, but a test run
// needs no installed provider and no development override.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"s3orchestrator": providerserver.NewProtocol6WithError(New("test")()),
}

// TestMain runs the orchestrator for the whole package.
//
// One container serves every test. They provision distinct names, and sharing
// one is what keeps a cold run from paying the image build more than once.
func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC") == "" {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	container, err := startOrchestrator(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start orchestrator: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate orchestrator: %v\n", err)
	}
	os.Exit(code)
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// testAccPreCheck fails a test that was run without the environment it needs,
// rather than letting it fail later with something less obvious.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	if testAccAddress == "" {
		t.Fatal("no orchestrator running: acceptance tests need TF_ACC=1")
	}
}

// testAccClient talks to the orchestrator directly, which is how a destroy
// check confirms a thing is gone rather than merely absent from state.
func testAccClient() *client.Client {
	return client.New(testAccAddress, testAccessKeyID, testSecretAccessKey)
}

// sameSet reports whether two permission lists hold the same names. The
// orchestrator renders them in its own fixed order, so comparing the slices as
// written would fail on ordering the configuration never chose.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	held := make(map[string]bool, len(got))
	for _, p := range got {
		held[p] = true
	}
	for _, p := range want {
		if !held[p] {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// startOrchestrator brings up the container and publishes its address.
func startOrchestrator(ctx context.Context) (testcontainers.Container, error) {
	image := os.Getenv(envImage)
	if image == "" {
		image = defaultImage
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{orchestratorPort},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(testConfig),
				ContainerFilePath: "/etc/s3-orchestrator/config.yaml",
				FileMode:          0o444,
			}},
			WaitingFor: wait.ForHTTP("/health/ready").
				WithPort(orchestratorPort).
				WithStartupTimeout(startupTimeout),
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}

	endpoint, err := container.PortEndpoint(ctx, orchestratorPort, "http")
	if err != nil {
		return nil, fmt.Errorf("resolve endpoint: %w", err)
	}
	testAccAddress = endpoint

	// The provider reads its address and keypair from the environment when the
	// provider block declares none, which is what every test configuration in
	// this package relies on: none of them can name a port chosen at runtime.
	for name, value := range map[string]string{
		envAddr:      endpoint,
		envAccessKey: testAccessKeyID,
		envSecretKey: testSecretAccessKey,
	} {
		if err := os.Setenv(name, value); err != nil {
			return nil, fmt.Errorf("set %s: %w", name, err)
		}
	}
	return container, nil
}
