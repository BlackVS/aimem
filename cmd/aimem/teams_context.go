package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"aimem/internal/teamguide"
)

const teamContextUsage = "usage: aimem teams context [--role worker|coordinator | --section ID]"

// teamContextCmd prints the team protocol guidance built into this binary:
// the index (ids, sizes, digests) by default, a role's complete required set
// or one section. It reads nothing but the binary: no checkout, hub,
// credential or network.
func teamContextCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("teams context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "print this role's complete required set")
	section := fs.String("section", "", "print one section by id")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v\n%s", err, teamContextUsage)
	}
	if fs.NArg() > 0 {
		return errors.New(teamContextUsage)
	}
	u, err := teamguide.Embedded()
	if err != nil {
		return err
	}
	var out string
	switch {
	case *role != "" && *section != "":
		return errors.New("give --role or --section, not both\n" + teamContextUsage)
	case *role != "":
		out, err = u.Role(*role, version)
	case *section != "":
		out, err = u.SectionText(*section, version)
	default:
		out, err = u.Index(version)
		out += "\n"
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, out)
	return err
}
