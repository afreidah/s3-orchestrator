// -------------------------------------------------------------------------------
// Admin CLI - object-tags
//
// Author: Alex Freidah
//
// Reads, replaces and clears one object's tag set. Tags are supplied as
// repeatable key=value pairs rather than a JSON body, so an operator can set
// them from a shell without quoting a document.
// -------------------------------------------------------------------------------

package adminctl

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// tagList collects repeated -tag key=value flags in the order given.
type tagList []adminapi.ObjectTag

// String renders the collected pairs, satisfying flag.Value.
func (t *tagList) String() string {
	parts := make([]string, len(*t))
	for i, tag := range *t {
		parts[i] = tag.Key + "=" + tag.Value
	}
	return strings.Join(parts, ",")
}

// Set parses one key=value pair. The value may contain "=", so only the first
// separator splits: a tag value is arbitrary text.
func (t *tagList) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	if key == "" {
		return fmt.Errorf("tag key must not be empty in %q", v)
	}
	*t = append(*t, adminapi.ObjectTag{Key: key, Value: value})
	return nil
}

// cmdObjectTags implements `s3-orchestrator admin object-tags -key=<key>`,
// with -set to replace the whole set and -clear to remove it. Without either
// it reads the current set.
func cmdObjectTags(args []string, c *client) int {
	fs := flag.NewFlagSet("object-tags", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	key := fs.String("key", "", "Object key (required)")
	clearTags := fs.Bool("clear", false, "Remove every tag from the object")
	var tags tagList
	fs.Var(&tags, "tag", "Tag to set as key=value; repeat for several. Replaces the whole set")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *key == "" {
		fmt.Fprintln(c.stderr, "error: -key is required")
		return 1
	}
	if *clearTags && len(tags) > 0 {
		fmt.Fprintln(c.stderr, "error: -clear and -tag are mutually exclusive")
		return 1
	}

	path := "/admin/api/objects/tags/" + url.PathEscape(*key)
	switch {
	case *clearTags:
		return c.delete(path, nil)
	case len(tags) > 0:
		body, err := json.Marshal(adminapi.ObjectTagsRequest{Tags: tags})
		if err != nil {
			fmt.Fprintf(c.stderr, "error: encode tags: %v\n", err)
			return 1
		}
		return c.put(path, string(body), nil)
	default:
		return c.get(path, nil)
	}
}
