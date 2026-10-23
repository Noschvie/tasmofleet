package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

const userUsage = `usage:
  tasmofleet user add [--role operator|admin] [--must-change] <name>   (password on stdin)
  tasmofleet user passwd [--create] [--role operator|admin] [--must-change] <name>   (password on stdin)
  tasmofleet user role <name> <operator|admin>
  tasmofleet user disable|enable|delete <name>
  tasmofleet user list
  tasmofleet sessions revoke [--user <name>]`

// accountsFor opens the data directory and returns the account manager plus
// the function that records the CLI action in the audit log.
func accountsFor(ctx context.Context) (acc *auth.Accounts, record func(action, target, detail string), closeFn func(), err error) {
	svc, err := openServices()
	if err != nil {
		return nil, nil, nil, err
	}
	acc = &auth.Accounts{Users: svc.Store, Sessions: svc.Store, Cookies: svc.Store, Clock: clock.Real{}}
	record = func(action, target, detail string) {
		svc.Audit.Record(ctx, model.AuditEvent{Actor: "cli", Action: action, Target: target, Detail: detail})
	}
	return acc, record, func() { _ = svc.Close() }, nil // exiting; the command result is what matters
}

func userCmd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	verb, args := args[0], args[1:]
	switch verb {
	case "add", "passwd":
		return userSetPassword(ctx, verb, args, stdin, stdout)
	case "role", "disable", "enable", "delete":
		return userSimple(ctx, verb, args, stdout)
	case "list":
		return userList(ctx, stdout)
	}
	return fmt.Errorf("unknown user command %q\n%s", verb, userUsage)
}

func userSetPassword(ctx context.Context, verb string, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("user "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	roleFlag := fs.String("role", "", "operator or admin")
	must := fs.Bool("must-change", false, "force a password change at the next login")
	create := fs.Bool("create", false, "create the account if it does not exist")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, userUsage)
	}
	if fs.NArg() != 1 {
		return errors.New(userUsage)
	}
	if verb == "add" && *create {
		return errors.New("user add: --create is for passwd")
	}
	name := auth.NormalizeUsername(fs.Arg(0))
	var role model.Role
	if *roleFlag != "" {
		var err error
		if role, err = auth.ParseRole(*roleFlag); err != nil {
			return err
		}
	}
	pw, err := readLine(stdin)
	if err != nil {
		return fmt.Errorf("read password from stdin: %w", err)
	}
	acc, record, done, err := accountsFor(ctx)
	if err != nil {
		return err
	}
	defer done()
	if verb == "add" {
		if role == model.RoleNone {
			role = model.RoleOperator
		}
		if err := acc.Add(ctx, name, pw, role, *must); err != nil {
			return err
		}
		record("user.add", "local:"+name, `{"role":"`+roleLabel(role)+`"}`)
		_, _ = fmt.Fprintf(stdout, "created %s (%s)\n", name, roleLabel(role))
		return nil
	}
	_, err = acc.Get(ctx, name)
	switch {
	case errors.Is(err, auth.ErrUserNotFound) && *create:
		if role == model.RoleNone {
			role = model.RoleOperator
		}
		if err := acc.Add(ctx, name, pw, role, *must); err != nil {
			return err
		}
		record("user.add", "local:"+name, `{"role":"`+roleLabel(role)+`"}`)
		_, _ = fmt.Fprintf(stdout, "created %s (%s)\n", name, roleLabel(role))
		return nil
	case err != nil:
		return err
	}
	if role != model.RoleNone { // recovery: "set-password" on an operator makes it an admin
		if err := acc.SetRole(ctx, name, role); err != nil {
			return err
		}
	}
	if err := acc.SetPassword(ctx, name, pw, *must); err != nil {
		return err
	}
	record("user.passwd", "local:"+name, "")
	_, _ = fmt.Fprintf(stdout, "password set for %s; its sessions and device cookies were revoked\n", name)
	return nil
}

func userSimple(ctx context.Context, verb string, args []string, stdout io.Writer) error {
	want := 1
	if verb == "role" {
		want = 2
	}
	if len(args) != want {
		return errors.New(userUsage)
	}
	name := auth.NormalizeUsername(args[0])
	acc, record, done, err := accountsFor(ctx)
	if err != nil {
		return err
	}
	defer done()
	switch verb {
	case "role":
		role, err := auth.ParseRole(args[1])
		if err != nil {
			return err
		}
		if err := acc.SetRole(ctx, name, role); err != nil {
			return err
		}
		record("user.role", "local:"+name, `{"role":"`+roleLabel(role)+`"}`)
		_, _ = fmt.Fprintf(stdout, "%s is now %s\n", name, roleLabel(role))
	case "disable":
		err = acc.SetDisabled(ctx, name, true)
	case "enable":
		err = acc.SetDisabled(ctx, name, false)
	case "delete":
		err = acc.Delete(ctx, name)
	}
	if err != nil {
		return err
	}
	if verb != "role" {
		record("user."+verb, "local:"+name, "")
		_, _ = fmt.Fprintf(stdout, "%s: %sd\n", name, strings.TrimSuffix(verb, "e")+"e")
	}
	return nil
}

func userList(ctx context.Context, stdout io.Writer) error {
	acc, _, done, err := accountsFor(ctx)
	if err != nil {
		return err
	}
	defer done()
	users, err := acc.List(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%-24s %-9s %s\n", "NAME", "ROLE", "STATE")
	for _, u := range users {
		state := "enabled"
		if u.Disabled {
			state = "disabled"
		}
		if u.MustChangePassword {
			state += ", must change password"
		}
		_, _ = fmt.Fprintf(stdout, "%-24s %-9s %s\n", u.Username, roleLabel(u.Role), state)
	}
	return nil
}

func roleLabel(r model.Role) string {
	if r == model.RoleAdmin {
		return "admin"
	}
	return "operator"
}

func sessionsCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "revoke" {
		return errors.New("usage: tasmofleet sessions revoke [--user <name>]")
	}
	fs := flag.NewFlagSet("sessions revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "", "end only this local user's sessions")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New("usage: tasmofleet sessions revoke [--user <name>]")
	}
	acc, record, done, err := accountsFor(ctx)
	if err != nil {
		return err
	}
	defer done()
	if *user != "" {
		n, err := acc.RevokeSessions(ctx, *user)
		if err != nil {
			return err
		}
		record("sessions.revoke_user", "local:"+auth.NormalizeUsername(*user), fmt.Sprintf(`{"sessions":%d}`, n))
		_, _ = fmt.Fprintf(stdout, "revoked %d sessions of %s\n", n, auth.NormalizeUsername(*user))
		return nil
	}
	n, err := (&auth.Sessions{Store: acc.Sessions, Clock: clock.Real{}}).RevokeAll(ctx)
	if err != nil {
		return err
	}
	record("sessions.revoke_all", "", fmt.Sprintf(`{"sessions":%d}`, n))
	_, _ = fmt.Fprintf(stdout, "revoked %d sessions\n", n)
	return nil
}
