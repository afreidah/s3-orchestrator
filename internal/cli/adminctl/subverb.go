// -------------------------------------------------------------------------------
// Admin CLI - Sub-verb Dispatch
//
// Author: Alex Freidah
//
// The provisioning commands take a verb after the command word, so `bucket
// create` and `bucket delete` read as one noun with several actions rather than
// as unrelated hyphenated commands. This is the shared parser for that shape:
// one table of verbs per noun, and a usage block generated from it, so a verb
// cannot be dispatched without being documented.
// -------------------------------------------------------------------------------

package adminctl

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// verb is one action under a noun, with the one-line description its usage
// block renders.
type verb struct {
	Name    string
	Summary string
	Run     handler
}

// nounCommand builds the handler for a noun that takes a verb. An absent,
// unknown, or "help" verb prints the usage block; anything else dispatches.
func nounCommand(noun string, verbs []verb) handler {
	return func(args []string, c *client) int {
		if len(args) == 0 || args[0] == "help" {
			printVerbUsage(c, noun, verbs)
			return exitCodeFor(len(args) == 0)
		}
		i := slices.IndexFunc(verbs, func(v verb) bool { return v.Name == args[0] })
		if i < 0 {
			fmt.Fprintf(c.stderr, "unknown %s verb: %s\n", noun, args[0])
			printVerbUsage(c, noun, verbs)
			return 1
		}
		return verbs[i].Run(args[1:], c)
	}
}

// exitCodeFor reports 1 when the verb was omitted and 0 when help was asked
// for, so a script that forgot an argument fails and `help` does not.
func exitCodeFor(missing bool) int {
	if missing {
		return 1
	}
	return 0
}

// printVerbUsage writes the verbs available under a noun.
func printVerbUsage(c *client, noun string, verbs []verb) {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: s3-orchestrator admin %s <verb> [flags]\n\nVerbs:\n", noun)
	writeVerbs(&b, verbs)
	fmt.Fprint(c.stderr, b.String())
}

// writeVerbs writes one aligned line per verb: its name, then its summary.
// The admin usage and every noun's usage share it.
func writeVerbs(w io.Writer, verbs []verb) {
	width := 0
	for _, v := range verbs {
		width = max(width, len(v.Name))
	}
	for _, v := range verbs {
		fmt.Fprintf(w, "  %-*s  %s\n", width, v.Name, v.Summary)
	}
}

// verbList joins the verbs' names the way a sentence lists them, as in
// "list, create or delete".
func verbList(verbs []verb) string {
	names := make([]string, len(verbs))
	for i := range verbs {
		names[i] = verbs[i].Name
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
