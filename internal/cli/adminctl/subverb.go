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
	width := 0
	for _, v := range verbs {
		width = max(width, len(v.Name))
	}
	for _, v := range verbs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, v.Name, v.Summary)
	}
	fmt.Fprint(c.stderr, b.String())
}
