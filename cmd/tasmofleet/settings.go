package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/nofuturekid/tasmofleet/internal/app"
)

const settingsUsage = `usage:
  tasmofleet settings export [file]    write the operational settings and policy rules as JSON (secrets are never exported)
  tasmofleet settings import <file>    validate and apply such a file (use - for stdin)
                                       it writes the database; a running server applies the changes only after a restart`

func settingsCmd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(settingsUsage)
	}
	svc, err := openServices()
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }() // exiting; the command result is what matters
	switch args[0] {
	case "export":
		if len(args) > 2 {
			return errors.New(settingsUsage)
		}
		if len(args) == 2 {
			f, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: the path is the operator's argument
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }() // the write error below is the one that matters
			return app.ExportSettings(ctx, svc, f)
		}
		return app.ExportSettings(ctx, svc, stdout)
	case "import":
		if len(args) != 2 {
			return errors.New(settingsUsage)
		}
		in := stdin
		if args[1] != "-" {
			f, err := os.Open(args[1]) //nolint:gosec // G304: the path is the operator's argument
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }() // read only
			in = f
		}
		return app.ImportSettings(ctx, svc, in, stdout)
	default:
		return errors.New(settingsUsage)
	}
}
