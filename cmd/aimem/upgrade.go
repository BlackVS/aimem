package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"aimem/internal/upgrade"
)

const upgradeUsage = `usage: aimem upgrade [--release vX.Y.Z]

Brings this installed client to the newest published release, or to the
release --release names (the only way to install an older one). The
binary is checked against the release's SHA256SUMS before anything is
changed; then the release's own installer replaces it, refreshes the
hooks, plugin and service, and restores the previous binary if the new
one does not come up. The first install stays the boot one-liner.`

func upgradeCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("aimem upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	release := fs.String("release", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(upgradeUsage)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return upgrade.Run(context.Background(), upgrade.DefaultSource(), upgrade.Options{
		Release: *release,
		Current: version,
		Self:    self,
		Out:     out,
	})
}
