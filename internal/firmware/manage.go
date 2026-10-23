package firmware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// ErrInUse is returned (wrapped in an *InUseError) when a delete is refused
// because an active or queued OTA job still refers to the artifact.
var ErrInUse = errors.New("firmware: artifact is in use by an OTA job")

// InUseError says which jobs hold the artifact.
type InUseError struct {
	Artifact model.Artifact
	Jobs     []model.JobID // may be empty if the job source cannot name them
}

func (e *InUseError) Error() string {
	if len(e.Jobs) == 0 {
		return fmt.Sprintf("firmware: %s/%s is in use by an OTA job", e.Artifact.Release, e.Artifact.Filename)
	}
	ids := make([]string, len(e.Jobs))
	for i, j := range e.Jobs {
		ids[i] = fmt.Sprint(int64(j))
	}
	return fmt.Sprintf("firmware: %s/%s is in use by job %s", e.Artifact.Release, e.Artifact.Filename, strings.Join(ids, ", "))
}

// Is lets errors.Is(err, ErrInUse) match.
func (e *InUseError) Is(target error) bool { return target == ErrInUse }

// Manager is implemented by the cache returned by New: management of the
// cached files for the firmware page.
type Manager interface {
	// ArtifactsInUse maps every artifact referenced by an active or queued
	// job (as target or as minimal) to the jobs holding it. The key is
	// present for an in-use artifact even if the jobs cannot be named.
	ArtifactsInUse(ctx context.Context) (map[model.ArtifactID][]model.JobID, error)
	// Delete removes one cached file and its index row. It refuses with an
	// *InUseError while a job needs the file, and also if that cannot be
	// determined (fail closed). It returns the deleted artifact.
	Delete(ctx context.Context, id model.ArtifactID) (model.Artifact, error)
	// DeleteUnused removes every cached file no job needs. Nothing is
	// deleted if the in-use lookup fails.
	DeleteUnused(ctx context.Context) (DeleteResult, error)
}

// DeleteResult is the outcome of DeleteUnused.
type DeleteResult struct {
	Deleted []model.Artifact
	Freed   int64 // bytes
	Kept    int   // artifacts left because a job holds them
}

func (c *cache) ArtifactsInUse(ctx context.Context) (map[model.ArtifactID][]model.JobID, error) {
	out := map[model.ArtifactID][]model.JobID{}
	switch {
	case c.opts.InUseBy != nil:
		m, err := c.opts.InUseBy(ctx)
		if err != nil {
			return nil, err
		}
		for id, jobs := range m {
			out[id] = append([]model.JobID(nil), jobs...)
		}
	case c.opts.InUse != nil:
		m, err := c.opts.InUse(ctx)
		if err != nil {
			return nil, err
		}
		for id, used := range m {
			if used {
				out[id] = nil
			}
		}
	}
	for _, jobs := range out {
		sort.Slice(jobs, func(i, j int) bool { return jobs[i] < jobs[j] })
	}
	return out, nil
}

func (c *cache) Delete(ctx context.Context, id model.ArtifactID) (model.Artifact, error) {
	a, err := c.store.GetArtifact(ctx, id)
	if err != nil {
		return model.Artifact{}, err
	}
	// The same per-file lock as Ensure: a delete never interleaves with a
	// download of the same file.
	lockKey := a.Release + "/" + a.Filename
	if a.Custom != "" {
		lockKey = "custom/" + a.Custom
	}
	unlock := c.lock(lockKey)
	defer unlock()
	if a, err = c.store.GetArtifact(ctx, id); err != nil { // deleted while we waited
		return model.Artifact{}, err
	}
	used, err := c.ArtifactsInUse(ctx)
	if err != nil {
		return model.Artifact{}, fmt.Errorf("firmware: delete refused, cannot determine artifacts in use: %w", err)
	}
	// Deleting an upload takes the minimal image copied next to it along.
	group := []model.Artifact{a}
	if a.Source == CustomSource {
		all, err := c.store.ListArtifacts(ctx)
		if err != nil {
			return a, err
		}
		for _, o := range all {
			if o.Custom == a.Custom && o.Release == a.Release && o.ID != a.ID && o.Source == customMinimalSource {
				group = append(group, o)
			}
		}
	}
	for _, g := range group {
		if jobs, busy := used[g.ID]; busy {
			return a, &InUseError{Artifact: g, Jobs: jobs}
		}
	}
	for _, g := range group {
		// Index first, as in Prune: a file without row is never served, a row
		// without file is repaired by Ensure.
		if err := c.store.DeleteArtifact(ctx, g.ID); err != nil {
			return a, err
		}
		if err := os.Remove(c.pathOf(g)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return a, fmt.Errorf("firmware: index row of %s/%s deleted but the file could not be removed: %w", g.Release, g.Filename, err)
		}
	}
	// Only succeeds when empty; a non-empty dir is left alone.
	if a.Custom != "" {
		c.removeCustomDirs(a)
	} else if err := os.Remove(filepath.Join(c.opts.Dir, a.Release)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.log.Debug("firmware: release directory kept", "release", a.Release, "err", err)
	}
	c.log.Info("firmware: deleted", "release", a.Release, "file", a.Filename, "size", a.Size)
	return a, nil
}

func (c *cache) DeleteUnused(ctx context.Context) (DeleteResult, error) {
	var res DeleteResult
	all, err := c.store.ListArtifacts(ctx)
	if err != nil {
		return res, err
	}
	if _, err := c.ArtifactsInUse(ctx); err != nil {
		return res, fmt.Errorf("firmware: nothing deleted, cannot determine artifacts in use: %w", err)
	}
	for _, a := range all {
		if IsCustomSource(a.Source) {
			continue // uploads are deleted one by one, never by "delete unused"
		}
		deleted, err := c.Delete(ctx, a.ID)
		var busy *InUseError
		switch {
		case err == nil:
			res.Deleted = append(res.Deleted, deleted)
			res.Freed += deleted.Size
		case errors.As(err, &busy):
			res.Kept++
		case errors.Is(err, store.ErrNotFound):
		default:
			return res, err
		}
	}
	return res, nil
}
