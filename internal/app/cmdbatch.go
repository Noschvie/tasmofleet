package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// principalResolver tells the OTA engine who a stored job creator is now. The
// identity is the pair (sign-in method, subject), because subjects are not
// unique across methods. A local account is read from the account store (role,
// disabled) and only under the reserved "local:" prefix, which no other method
// may use. Single sign-on and header users are known by the role in their
// principal record (written at sign-in, for header users at most every few
// minutes), and only if the record was written by the same method; an unknown
// method, a missing record, another method's record or no role fails closed.
type principalResolver struct {
	local interface {
		GetLocalUser(ctx context.Context, username string) (store.LocalUser, error)
	}
	dir store.PrincipalStore
}

const localSubjectPrefix = "local:"

func (r principalResolver) Current(ctx context.Context, method, subject string) (model.Principal, error) {
	switch method {
	case auth.ModeLocal:
		name, ok := strings.CutPrefix(subject, localSubjectPrefix)
		if !ok {
			return model.Principal{}, errors.New("not a local account")
		}
		u, err := r.local.GetLocalUser(ctx, name)
		if err != nil {
			return model.Principal{}, fmt.Errorf("the account %q is gone", name)
		}
		if u.Disabled {
			return model.Principal{}, fmt.Errorf("the account %q is disabled", name)
		}
		return model.Principal{Subject: subject, Name: name, Role: u.Role, Method: auth.ModeLocal}, nil
	case auth.ModeOIDC, auth.ModeHeader:
	default:
		return model.Principal{}, errors.New("the sign-in method of the creator is unknown")
	}
	if strings.HasPrefix(subject, localSubjectPrefix) {
		return model.Principal{}, errors.New("the subject belongs to the local accounts")
	}
	recs, err := r.dir.ListPrincipals(ctx)
	if err != nil {
		return model.Principal{}, err
	}
	wantKind := auth.MethodHeader
	if method == auth.ModeOIDC {
		wantKind = auth.MethodSSO
	}
	for _, rec := range recs {
		if rec.Subject != subject {
			continue
		}
		if rec.Kind != wantKind {
			return model.Principal{}, errors.New("the subject is recorded for another sign-in method")
		}
		role, ok := map[string]model.Role{"admin": model.RoleAdmin, "operator": model.RoleOperator}[rec.Role]
		if !ok {
			return model.Principal{}, errors.New("no role is known for this user")
		}
		return model.Principal{Subject: subject, Name: rec.DisplayName, Role: role, Method: method}, nil
	}
	return model.Principal{}, errors.New("this user has not signed in")
}

// lockedCommandBatch starts "Send command" transactions. Like lockedPolicyFix
// it takes the lock of every device while the jobs are created; a device that
// somebody else holds is left out with the holder named.
type lockedCommandBatch struct {
	e *ota.Engine
	g deviceGuard
}

func (l lockedCommandBatch) Submit(ctx context.Context, actor string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error) {
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	var free []ota.PolicyRequest
	var skipped []ota.Refusal
	seen := map[model.DeviceID]bool{}
	for _, r := range reqs {
		if seen[r.DeviceID] {
			free = append(free, r) // the engine reports the duplicate itself
			continue
		}
		seen[r.DeviceID] = true
		release, holder, ok := l.g.locks.TryAcquire(r.DeviceID, "command")
		if !ok {
			skipped = append(skipped, ota.Refusal{DeviceID: r.DeviceID, Reason: devlock.BusyError{Holder: holder}.Error()})
			continue
		}
		releases = append(releases, release)
		free = append(free, r)
	}
	if len(free) == 0 {
		return 0, skipped, ota.ErrNothingToFix
	}
	id, more, err := l.e.SubmitCommand(ctx, actor, free)
	return id, append(skipped, more...), err
}

// lockedBackupBatch starts "Back up" transactions. Like lockedCommandBatch it
// takes the lock of every device while the jobs are created; a device that
// somebody else holds is left out with the holder named.
type lockedBackupBatch struct {
	e *ota.Engine
	g deviceGuard
}

func (l lockedBackupBatch) Submit(ctx context.Context, actor string, ids []model.DeviceID, opt ota.BackupOptions) (model.BatchID, []ota.Refusal, error) {
	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	var free []model.DeviceID
	var skipped []ota.Refusal
	seen := map[model.DeviceID]bool{}
	for _, id := range ids {
		if seen[id] {
			free = append(free, id) // the engine reports the duplicate itself
			continue
		}
		seen[id] = true
		release, holder, ok := l.g.locks.TryAcquire(id, "backup")
		if !ok {
			skipped = append(skipped, ota.Refusal{DeviceID: id, Reason: devlock.BusyError{Holder: holder}.Error()})
			continue
		}
		releases = append(releases, release)
		free = append(free, id)
	}
	if len(free) == 0 {
		return 0, skipped, ota.ErrNothingToFix
	}
	id, more, err := l.e.SubmitBackupWith(ctx, actor, free, opt)
	return id, append(skipped, more...), err
}
