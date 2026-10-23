package app

import (
	"context"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// activeJobLister is the part of the job store the firmware cache needs.
type activeJobLister interface {
	ActiveJobs(ctx context.Context) ([]model.Job, error)
}

// jobsByArtifact tells the firmware cache which artifacts (as target or as
// tasmota-minimal) are held by active or queued OTA jobs, and by which job.
// The cache refuses to delete them and names the jobs.
func jobsByArtifact(st activeJobLister) func(context.Context) (map[model.ArtifactID][]model.JobID, error) {
	return func(ctx context.Context) (map[model.ArtifactID][]model.JobID, error) {
		jobs, err := st.ActiveJobs(ctx)
		if err != nil {
			return nil, err
		}
		used := map[model.ArtifactID][]model.JobID{}
		for _, j := range jobs {
			for _, id := range []model.ArtifactID{j.TargetArtifact, j.MinimalArtifact} {
				if id != 0 {
					used[id] = append(used[id], j.ID)
				}
			}
		}
		return used, nil
	}
}
