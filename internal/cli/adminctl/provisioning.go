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
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/output"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The flag names and messages the provisioning verbs share.
const (
	flagUser      = "user"
	flagBucket    = "bucket"
	flagKind      = "kind"
	flagName      = "name"
	flagID        = "id"
	flagAccessKey = "access-key"

	defaultGrantKind = "bucket"

	usageBucketName = "Bucket name (required)"
	usageUserID     = "User ID (required)"
	usageGrantKind  = "Resource kind: bucket, backend or orchestrator"
	usageGrantName  = "Resource name, or * for every one of its kind; omitted for the orchestrator"

	usageGrantPermissions = "Comma-separated permissions; all for every data-plane one, " +
		"admin-all for every control-plane one"

	usageIssueAccessKey = "Access key ID to register; omit with -secret-key to mint a keypair instead"
	usageIssueSecretKey = "Secret access key to register; required with -access-key"

	errKeypairIncomplete = "error: -access-key and -secret-key must both be given, or both omitted"
	errGrantNameRequired = "error: -name is required for a bucket or backend grant"

	colSource = "Source"
)

// -------------------------------------------------------------------------
// DISPATCH
// -------------------------------------------------------------------------

// The verbs each provisioning noun takes, as in `s3-orchestrator admin bucket
// create`. The admin usage builds each noun's summary from its table.
var (
	bucketVerbs = []verb{
		{"list", "List every virtual bucket, from the config file and the store", bucketList},
		{"create", "Declare a virtual bucket in the store", bucketCreate},
		{"delete", "Remove a stored virtual bucket that holds no objects and no grants", bucketDelete},
	}
	userVerbs = []verb{
		{"list", "List every identity and the buckets it reaches", userList},
		{"create", "Declare an identity credentials can be issued against", userCreate},
		{"rename", "Change the name an identity is read by", userRename},
		{"delete", "Remove an identity that holds no credentials and no grants", userDelete},
	}
	credentialVerbs = []verb{
		{"list", "List every keypair, without its secret", credentialList},
		{"issue", "Mint or register a keypair for a user and print it", credentialIssue},
		{"revoke", "Revoke one keypair, leaving its siblings working", credentialRevoke},
	}
	grantVerbs = []verb{
		{"add", "Let a user reach a bucket, a backend or the orchestrator", grantAdd},
		{"set", "Declare exactly what a user reaches on one resource", grantSet},
		{"remove", "Withdraw one user's access to one resource", grantRemove},
	}
)

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// bucketList renders every virtual bucket either source declares.
func bucketList(_ []string, c *client) int {
	return c.get(adminapi.PathProvisioning, renderBuckets)
}

// bucketCreate declares a virtual bucket in the store.
func bucketCreate(args []string, c *client) int {
	fs := c.flags("bucket create")
	name := fs.String(flagName, "", usageBucketName)
	maxMultipart := fs.Int("max-multipart", 0, "Cap concurrent multipart uploads; 0 is unlimited")
	if !c.parse(fs, args, flagName) {
		return 1
	}
	return c.sendJSON(http.MethodPost, adminapi.PathProvBuckets, adminapi.CreateBucketRequest{
		Name:                *name,
		MaxMultipartUploads: *maxMultipart,
	}, nil)
}

// bucketDelete removes a stored virtual bucket.
func bucketDelete(args []string, c *client) int {
	fs := c.flags("bucket delete")
	name := fs.String(flagName, "", usageBucketName)
	if !c.parse(fs, args, flagName) {
		return 1
	}
	return c.delete(adminapi.Path(adminapi.PathProvBuckets, *name))
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// userList renders every identity and the buckets it reaches.
func userList(_ []string, c *client) int {
	return c.get(adminapi.PathProvisioning, renderUsers)
}

// userCreate declares an identity. The server generates the id, so the
// response is rendered rather than discarded: every later verb names the user
// by it.
func userCreate(args []string, c *client) int {
	fs := c.flags("user create")
	name := fs.String(flagName, "", "User name (required)")
	if !c.parse(fs, args, flagName) {
		return 1
	}
	return c.sendJSON(http.MethodPost, adminapi.PathProvUsers, adminapi.CreateUserRequest{Name: *name}, nil)
}

// userRename changes the name an identity is read by.
//
// The ID does not move, so credentials and grants hanging off the user are
// untouched. Correcting a name is otherwise impossible: a delete is refused
// while the user holds either of those.
func userRename(args []string, c *client) int {
	fs := c.flags("user rename")
	id := fs.String(flagID, "", usageUserID)
	name := fs.String(flagName, "", "New name for the identity (required)")
	if !c.parse(fs, args, flagID, flagName) {
		return 1
	}
	return c.sendJSON(http.MethodPatch, adminapi.Path(adminapi.PathProvUsers, *id), adminapi.RenameUserRequest{Name: *name}, nil)
}

// userDelete removes an identity.
func userDelete(args []string, c *client) int {
	fs := c.flags("user delete")
	id := fs.String(flagID, "", usageUserID)
	if !c.parse(fs, args, flagID) {
		return 1
	}
	return c.delete(adminapi.Path(adminapi.PathProvUsers, *id))
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// credentialList renders every keypair. The server does not return secrets, so
// there is nothing here to withhold.
func credentialList(_ []string, c *client) int {
	return c.get(adminapi.PathProvisioning, renderCredentials)
}

// credentialIssue records a keypair against a user and prints it. A minted
// secret goes to stdout only and cannot be read back later; a caller that
// loses it issues a replacement. Supplying a keypair registers that one
// instead, so the command can be rerun without minting a second credential.
func credentialIssue(args []string, c *client) int {
	fs := c.flags("credential issue")
	user := fs.String(flagUser, "", "User ID to issue against (required)")
	label := fs.String("label", "", "Human label recording what holds this keypair")
	accessKey := fs.String(flagAccessKey, "", usageIssueAccessKey)
	secretKey := fs.String("secret-key", "", usageIssueSecretKey)
	if !c.parse(fs, args, flagUser) {
		return 1
	}
	if (*accessKey == "") != (*secretKey == "") {
		fmt.Fprintln(c.stderr, errKeypairIncomplete)
		return 1
	}
	return c.sendJSON(http.MethodPost, adminapi.PathProvCreds, adminapi.CreateCredentialRequest{
		UserID:          *user,
		Label:           *label,
		AccessKeyID:     *accessKey,
		SecretAccessKey: *secretKey,
	}, renderNewCredential)
}

// credentialRevoke revokes one keypair.
func credentialRevoke(args []string, c *client) int {
	fs := c.flags("credential revoke")
	accessKey := fs.String(flagAccessKey, "", "Access key ID to revoke (required)")
	if !c.parse(fs, args, flagAccessKey) {
		return 1
	}
	return c.delete(adminapi.Path(adminapi.PathProvCreds, *accessKey))
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// grantFlagValues holds what the flags every grant verb names its target with
// parse into.
type grantFlagValues struct {
	user   *string
	kind   *string
	name   *string
	bucket *string
}

// grantFlags registers the flags every grant verb shares and returns the set,
// so a verb can add the ones only it takes before parsing.
func grantFlags(verb string, c *client) (*flag.FlagSet, *grantFlagValues) {
	fs := c.flags(verb)
	return fs, &grantFlagValues{
		user:   fs.String(flagUser, "", usageUserID),
		kind:   fs.String(flagKind, defaultGrantKind, usageGrantKind),
		name:   fs.String(flagName, "", usageGrantName),
		bucket: fs.String(flagBucket, "", usageBucketName),
	}
}

// resolveGrant parses the arguments and reports the user and resource they
// name, or false after printing the reason on stderr. -bucket is an alias for
// a bucket grant. The kind is normalized here as well as on the server, so the
// "instance" alias for the orchestrator is not asked for a name.
func resolveGrant(
	fs *flag.FlagSet, args []string, c *client, v *grantFlagValues,
) (string, core.Resource, bool) {
	if !c.parse(fs, args, flagUser) {
		return "", core.Resource{}, false
	}
	resource := core.Resource{
		Kind: core.ParseResourceKind(*v.kind),
		Name: cmp.Or(*v.name, *v.bucket),
	}
	if resource.Name == "" && resource.Kind != core.ResourceOrchestrator {
		fmt.Fprintln(c.stderr, errGrantNameRequired)
		return "", core.Resource{}, false
	}
	return *v.user, resource, true
}

// grantPath addresses one grant. The orchestrator resource has no name, so its
// path carries a placeholder segment the server ignores.
func grantPath(user string, r core.Resource) string {
	path := adminapi.Path(adminapi.PathProvGrants, user, cmp.Or(r.Name, string(core.ResourceOrchestrator)))
	return adminclient.WithQuery(path, url.Values{flagKind: {string(r.Kind)}})
}

// grantAdd lets a user reach a resource, with the permissions that reach
// carries.
func grantAdd(args []string, c *client) int {
	fs, v := grantFlags("grant add", c)
	perms := fs.String("permissions", "all", usageGrantPermissions)
	user, resource, ok := resolveGrant(fs, args, c, v)
	if !ok {
		return 1
	}
	return c.sendJSON(http.MethodPost, adminapi.PathProvGrants, adminapi.CreateGrantRequest{
		UserID:      user,
		Kind:        string(resource.Kind),
		Name:        resource.Name,
		Permissions: splitPermissions(*perms),
	}, nil)
}

// splitPermissions turns the flag value into the list the request carries. The
// server owns which names are valid, so this only splits and trims: rejecting
// here as well would put the same list in two places to drift apart.
func splitPermissions(s string) []string {
	var out []string
	for field := range strings.SplitSeq(s, ",") {
		if name := strings.TrimSpace(field); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// grantSet declares exactly what a user reaches on one resource. Unlike
// `add`, it upserts, so rerunning it converges instead of failing when the
// grant already exists.
func grantSet(args []string, c *client) int {
	fs, v := grantFlags("grant set", c)
	perms := fs.String("permissions", "all", usageGrantPermissions)
	user, resource, ok := resolveGrant(fs, args, c, v)
	if !ok {
		return 1
	}
	return c.sendJSON(http.MethodPut, grantPath(user, resource), adminapi.SetGrantRequest{Permissions: splitPermissions(*perms)}, nil)
}

// grantRemove withdraws one user's access to one resource.
func grantRemove(args []string, c *client) int {
	fs, v := grantFlags("grant remove", c)
	user, resource, ok := resolveGrant(fs, args, c, v)
	if !ok {
		return 1
	}
	return c.delete(grantPath(user, resource))
}

// -------------------------------------------------------------------------
// RENDERERS
// -------------------------------------------------------------------------

// provisioningTable renders one slice of the provisioning listing as a table
// under headers, followed by the notices the merge of the two sources found.
func provisioningTable(headers []string, rowsOf func(*adminapi.ProvisioningResponse) [][]string) renderFunc {
	return func(w io.Writer, body []byte) error {
		var p adminapi.ProvisioningResponse
		if err := json.Unmarshal(body, &p); err != nil {
			return err
		}
		if err := output.Table(w, headers, rowsOf(&p)); err != nil {
			return err
		}
		return writeConfigNote(w, p.Notices)
	}
}

// renderBuckets renders the bucket slice of the provisioning response.
var renderBuckets = provisioningTable([]string{"Bucket", "Multipart", colSource},
	func(p *adminapi.ProvisioningResponse) [][]string {
		rows := make([][]string, len(p.Buckets))
		for i, b := range p.Buckets {
			rows[i] = []string{b.Name, output.MultipartCap(b.MaxMultipartUploads), b.Source}
		}
		return rows
	})

// renderUsers renders the user slice, each with the buckets it reaches.
var renderUsers = provisioningTable([]string{"User ID", "Name", "Grants", colSource},
	func(p *adminapi.ProvisioningResponse) [][]string {
		rows := make([][]string, len(p.Users))
		for i, u := range p.Users {
			rows[i] = []string{u.ID, u.Name, renderGrants(&u), u.Source}
		}
		return rows
	})

// renderCredentials renders the credential slice.
var renderCredentials = provisioningTable([]string{"Access key", "User ID", "Label", colSource},
	func(p *adminapi.ProvisioningResponse) [][]string {
		rows := make([][]string, len(p.Credentials))
		for i, cred := range p.Credentials {
			rows[i] = []string{cred.AccessKeyID, cred.UserID, cred.Label, cred.Source}
		}
		return rows
	})

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
// "bucket:photos(read,write)". A user holding nothing renders empty. It falls
// back to the bucket list when the server sent no grants (an older instance).
func renderGrants(u *adminapi.User) string {
	if len(u.Grants) == 0 {
		return strings.Join(u.Buckets, " ")
	}
	out := make([]string, 0, len(u.Grants))
	for _, g := range u.Grants {
		resource := core.Resource{Kind: core.ParseResourceKind(g.Kind), Name: g.Name}
		out = append(out, resource.String()+"("+shorthand(g.Permissions)+")")
	}
	return strings.Join(out, " ")
}

// shorthand renders a permission list the way the stored form does, collapsing
// a complete set to the one word that names it. Only the CLI listing
// abbreviates; the API returns every name.
func shorthand(perms []string) string {
	for _, full := range []core.PermissionSet{core.PermAll, core.PermAdminAll} {
		if slices.Equal(perms, full.Names()) {
			return full.String()
		}
	}
	return strings.Join(perms, ",")
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
