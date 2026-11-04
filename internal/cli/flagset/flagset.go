// -------------------------------------------------------------------------------
// CLI Flag Sets
//
// Author: Alex Freidah
//
// One way for every subcommand to parse its flags. Parsing never exits the
// process: the caller gets the exit code back, so a help request stops with 0,
// a bad flag or a missing required one with 1, and a command stays testable.
// -------------------------------------------------------------------------------

// Package flagset parses subcommand flags without exiting the process.
package flagset

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// New starts a command's flag set, reporting parse errors and usage on w.
func New(name string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}

// Parse reads args into fs and refuses a run that leaves any of the required
// flags empty, naming the first one missing. It reports whether the command may
// go on, and otherwise the code to exit with.
func Parse(fs *flag.FlagSet, args []string, required ...string) (exit int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 1, false
	}
	for _, name := range required {
		if fs.Lookup(name).Value.String() == "" {
			fmt.Fprintf(fs.Output(), "error: -%s is required\n", name)
			return 1, false
		}
	}
	return 0, true
}
