// -------------------------------------------------------------------------------
// Admin CLI - bucket, user, credential and grant
//
// Author: Alex Freidah
//
// Declaring virtual buckets and the credentials that reach them without editing
// the config file. Every listing marks which entries the config file declares,
// because those are the ones the server refuses to change.
//
// The three listings are slices of one response: the server returns buckets,
// users and credentials together, so each verb renders the part it is named for
// and JSON mode hands back the whole document.
// -------------------------------------------------------------------------------

package adminctl

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The provisioning routes and the flag names and messages the verbs share.
const (
	pathProvisioning = "/admin/api/provisioning"
	pathProvBuckets  = pathProvisioning + "/buckets"
	pathProvUsers    = pathProvisioning + "/users"
	pathProvCreds    = pathProvisioning + "/credentials"
	pathProvGrants   = pathProvisioning + "/grants"

	flagUser   = "user"
	flagBucket = "bucket"
	flagKind   = "kind"
	flagName   = "name"

	defaultGrantKind = "bucket"

	usageBucketName = "Bucket name (required)"
	usageUserID     = "User ID (required)"
	usageGrantKind  = "Resource kind: bucket, backend or instance"
	usageGrantName  = "Resource name, or * for every one of its kind; omitted for the instance"

	usageGrantPermissions = "Comma-separated permissions; all for every data-plane one, " +
		"admin-all for every control-plane one"

	errNameRequired      = "error: -name is required"
	errUserRequired      = "error: -user is required"
	errBucketRequired    = "error: -bucket is required"
	errGrantNameRequired = "error: -name is required for a bucket or backend grant"

	colSource = "Source"
)

// -------------------------------------------------------------------------
// DISPATCH
// -------------------------------------------------------------------------

// cmdBucket implements `s3-orchestrator admin bucket <verb>`.
var cmdBucket = nounCommand("bucket", []verb{
	{"list", "List every virtual bucket, from the config file and the store", bucketList},
	{"create", "Declare a virtual bucket in the store", bucketCreate},
	{"delete", "Remove a stored virtual bucket that holds no objects and no grants", bucketDelete},
})

// cmdUser implements `s3-orchestrator admin user <verb>`.
var cmdUser = nounCommand("user", []verb{
	{"list", "List every identity and the buckets it reaches", userList},
	{"create", "Declare an identity credentials can be issued against", userCreate},
	{"delete", "Remove an identity that holds no credentials and no grants", userDelete},
})

// cmdCredential implements `s3-orchestrator admin credential <verb>`.
var cmdCredential = nounCommand("credential", []verb{
	{"list", "List every keypair, without its secret", credentialList},
	{"issue", "Mint a keypair for a user and print it once", credentialIssue},
	{"revoke", "Revoke one keypair, leaving its siblings working", credentialRevoke},
})

// cmdGrant implements `s3-orchestrator admin grant <verb>`.
var cmdGrant = nounCommand("grant", []verb{
	{"add", "Let a user reach a bucket, a backend or the instance", grantAdd},
	{"remove", "Withdraw one user's access to one resource", grantRemove},
})

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// bucketList renders every virtual bucket either source declares.
func bucketList(_ []string, c *client) int {
	return c.get(pathProvisioning, renderBuckets)
}

// bucketCreate declares a virtual bucket in the store.
func bucketCreate(args []string, c *client) int {
	fs := flag.NewFlagSet("bucket create", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	name := fs.String("name", "", usageBucketName)
	maxMultipart := fs.Int("max-multipart", 0, "Cap concurrent multipart uploads; 0 is unlimited")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *name == "" {
		fmt.Fprintln(c.stderr, errNameRequired)
		return 1
	}
	body, err := json.Marshal(adminapi.CreateBucketRequest{
		Name:                *name,
		MaxMultipartUploads: *maxMultipart,
	})
	if err != nil {
		fmt.Fprintf(c.stderr, "error: encode request: %v\n", err)
		return 1
	}
	return c.post(pathProvBuckets, string(body), nil)
}

// bucketDelete removes a stored virtual bucket.
func bucketDelete(args []string, c *client) int {
	fs := flag.NewFlagSet("bucket delete", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	name := fs.String("name", "", usageBucketName)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *name == "" {
		fmt.Fprintln(c.stderr, errNameRequired)
		return 1
	}
	return c.delete(pathProvBuckets+"/"+url.PathEscape(*name), nil)
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// userList renders every identity and the buckets it reaches.
func userList(_ []string, c *client) int {
	return c.get(pathProvisioning, renderUsers)
}

// userCreate declares an identity. The server generates the id, so the
// response is rendered rather than discarded: every later verb names the user
// by it.
func userCreate(args []string, c *client) int {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	name := fs.String("name", "", "User name (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *name == "" {
		fmt.Fprintln(c.stderr, errNameRequired)
		return 1
	}
	body, err := json.Marshal(adminapi.CreateUserRequest{Name: *name})
	if err != nil {
		fmt.Fprintf(c.stderr, "error: encode request: %v\n", err)
		return 1
	}
	return c.post(pathProvUsers, string(body), nil)
}

// userDelete removes an identity.
func userDelete(args []string, c *client) int {
	fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	id := fs.String("id", "", usageUserID)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *id == "" {
		fmt.Fprintln(c.stderr, "error: -id is required")
		return 1
	}
	return c.delete(pathProvUsers+"/"+url.PathEscape(*id), nil)
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// credentialList renders every keypair. The server does not return secrets, so
// there is nothing here to withhold.
func credentialList(_ []string, c *client) int {
	return c.get(pathProvisioning, renderCredentials)
}

// credentialIssue mints a keypair and prints it once.
//
// The secret reaches stdout and nowhere else, so the output can be piped into a
// secret store without the value passing through a log line. Nothing reads it
// back afterwards: a caller that loses it issues a replacement.
func credentialIssue(args []string, c *client) int {
	fs := flag.NewFlagSet("credential issue", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	user := fs.String(flagUser, "", "User ID to issue against (required)")
	label := fs.String("label", "", "Human label recording what holds this keypair")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *user == "" {
		fmt.Fprintln(c.stderr, errUserRequired)
		return 1
	}
	body, err := json.Marshal(adminapi.CreateCredentialRequest{UserID: *user, Label: *label})
	if err != nil {
		fmt.Fprintf(c.stderr, "error: encode request: %v\n", err)
		return 1
	}
	return c.post(pathProvCreds, string(body), renderNewCredential)
}

// credentialRevoke revokes one keypair.
func credentialRevoke(args []string, c *client) int {
	fs := flag.NewFlagSet("credential revoke", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	accessKey := fs.String("access-key", "", "Access key ID to revoke (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *accessKey == "" {
		fmt.Fprintln(c.stderr, "error: -access-key is required")
		return 1
	}
	return c.delete(pathProvCreds+"/"+url.PathEscape(*accessKey), nil)
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// grantAdd lets a user reach a resource, with the permissions that reach
// carries.
func grantAdd(args []string, c *client) int {
	fs := flag.NewFlagSet("grant add", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	user := fs.String(flagUser, "", usageUserID)
	kind := fs.String(flagKind, defaultGrantKind, usageGrantKind)
	name := fs.String(flagName, "", usageGrantName)
	bucket := fs.String(flagBucket, "", usageBucketName)
	perms := fs.String("permissions", "all", usageGrantPermissions)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *user == "" {
		fmt.Fprintln(c.stderr, errUserRequired)
		return 1
	}
	// -bucket is the older spelling of a bucket grant and stays as an alias, so
	// an operator's existing scripts keep working now that grants reach past
	// buckets.
	resourceName := cmp.Or(*name, *bucket)
	if resourceName == "" && *kind != string(core.ResourceInstance) {
		fmt.Fprintln(c.stderr, errGrantNameRequired)
		return 1
	}

	body, err := json.Marshal(adminapi.CreateGrantRequest{
		UserID:      *user,
		Kind:        *kind,
		Name:        resourceName,
		Permissions: splitPermissions(*perms),
	})
	if err != nil {
		fmt.Fprintf(c.stderr, "error: encode request: %v\n", err)
		return 1
	}
	return c.post(pathProvGrants, string(body), nil)
}

// splitPermissions turns the flag value into the list the request carries. The
// server owns which names are valid, so this only splits and trims: rejecting
// here as well would put the same list in two places to drift apart.
func splitPermissions(s string) []string {
	var out []string
	for _, field := range strings.Split(s, ",") {
		if name := strings.TrimSpace(field); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// grantRemove withdraws one user's access to one resource.
func grantRemove(args []string, c *client) int {
	fs := flag.NewFlagSet("grant remove", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	user := fs.String(flagUser, "", usageUserID)
	kind := fs.String(flagKind, defaultGrantKind, usageGrantKind)
	name := fs.String(flagName, "", usageGrantName)
	bucket := fs.String(flagBucket, "", usageBucketName)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *user == "" {
		fmt.Fprintln(c.stderr, errUserRequired)
		return 1
	}
	resourceName := cmp.Or(*name, *bucket)
	if resourceName == "" && *kind != string(core.ResourceInstance) {
		fmt.Fprintln(c.stderr, errGrantNameRequired)
		return 1
	}
	// The instance has no name, so the path carries a placeholder segment the
	// server discards once the kind says which resource is meant.
	path := pathProvGrants + "/" + url.PathEscape(*user) + "/" +
		url.PathEscape(cmp.Or(resourceName, string(core.ResourceInstance))) +
		"?" + flagKind + "=" + url.QueryEscape(*kind)
	return c.delete(path, nil)
}

// -------------------------------------------------------------------------
// RENDERERS
// -------------------------------------------------------------------------

// renderBuckets renders the bucket slice of the provisioning response.
func renderBuckets(w io.Writer, body []byte) error {
	p, err := decodeProvisioning(body)
	if err != nil {
		return err
	}
	rows := make([][]string, len(p.Buckets))
	for i, b := range p.Buckets {
		rows[i] = []string{b.Name, multipartLimit(b.MaxMultipartUploads), b.Source}
	}
	if err := output.Table(w, []string{"Bucket", "Multipart", colSource}, rows); err != nil {
		return err
	}
	return writeConfigNote(w, p.Notices)
}

// renderUsers renders the user slice, each with the buckets it reaches.
func renderUsers(w io.Writer, body []byte) error {
	p, err := decodeProvisioning(body)
	if err != nil {
		return err
	}
	rows := make([][]string, len(p.Users))
	for i, u := range p.Users {
		rows[i] = []string{u.ID, u.Name, renderGrants(&u), u.Source}
	}
	if err := output.Table(w, []string{"User ID", "Name", "Grants", colSource}, rows); err != nil {
		return err
	}
	return writeConfigNote(w, p.Notices)
}

// renderCredentials renders the credential slice.
func renderCredentials(w io.Writer, body []byte) error {
	p, err := decodeProvisioning(body)
	if err != nil {
		return err
	}
	rows := make([][]string, len(p.Credentials))
	for i, cred := range p.Credentials {
		rows[i] = []string{cred.AccessKeyID, cred.UserID, cred.Label, cred.Source}
	}
	if err := output.Table(w, []string{"Access key", "User ID", "Label", colSource}, rows); err != nil {
		return err
	}
	return writeConfigNote(w, p.Notices)
}

// renderNewCredential prints a minted keypair. Both halves go to stdout so the
// output can be captured whole; this is the only time the secret is available.
func renderNewCredential(w io.Writer, body []byte) error {
	var c adminapi.CreateCredentialResponse
	if err := json.Unmarshal(body, &c); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w,
		"access_key_id:     %s\nsecret_access_key: %s\nuser_id:           %s\n\n"+
			"The secret is not stored anywhere it can be read back. Capture it now.\n",
		c.AccessKeyID, c.SecretAccessKey, c.UserID)
	return err
}

// renderGrants renders what a user reaches and what each reach carries, as
// "bucket:photos(read,write)". A user holding nothing renders empty, which is
// what an identity created but not yet granted anything is.
//
// Falls back to the bucket list when the server sent no grants, so a listing
// read from an older instance still says which buckets are reached.
func renderGrants(u *adminapi.User) string {
	if len(u.Grants) == 0 {
		return strings.Join(u.Buckets, " ")
	}
	out := make([]string, 0, len(u.Grants))
	for _, g := range u.Grants {
		resource := core.Resource{Kind: core.ResourceKind(g.Kind), Name: g.Name}
		out = append(out, resource.String()+"("+shorthand(g.Permissions)+")")
	}
	return strings.Join(out, " ")
}

// shorthand renders a permission list the way the stored form does, collapsing
// a complete set to the one word that names it.
//
// A full control-plane set is ten names, which is wider than the terminal a
// listing is read in. The API keeps every name, because a caller parsing it
// should not have to know what the shorthand expands to.
func shorthand(perms []string) string {
	for _, full := range []core.PermissionSet{core.PermAll, core.PermAdminAll} {
		if slices.Equal(perms, full.Names()) {
			return full.String()
		}
	}
	return strings.Join(perms, ",")
}

// decodeProvisioning parses the shared listing response.
func decodeProvisioning(body []byte) (adminapi.ProvisioningResponse, error) {
	var p adminapi.ProvisioningResponse
	err := json.Unmarshal(body, &p)
	return p, err
}

// multipartLimit renders a bucket's multipart cap, spelling out that zero means
// no cap rather than no uploads.
func multipartLimit(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// writeConfigNote appends what the server found worth reporting about the two
// sources it merged, so an operator sees a dangling grant without reading the
// server log.
func writeConfigNote(w io.Writer, notices []adminapi.Notice) error {
	if len(notices) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, n := range notices {
		if _, err := fmt.Fprintf(w, "notice: %s: %s\n", n.Kind, n.Detail); err != nil {
			return err
		}
	}
	return nil
}
