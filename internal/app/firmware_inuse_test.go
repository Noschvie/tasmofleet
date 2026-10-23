package app

import (
	"context"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type fakeJobs struct {
	jobs []model.Job
	err  error
}

func (f fakeJobs) ActiveJobs(context.Context) ([]model.Job, error) { return f.jobs, f.err }

// Why: a staged job needs two files at once, and the jobs of a batch share
// them; the delete refusal must name every holder of either.
func TestJobsByArtifactCoversTargetAndMinimal(t *testing.T) {
	got, err := jobsByArtifact(fakeJobs{jobs: []model.Job{
		{ID: 1, TargetArtifact: 10, MinimalArtifact: 11},
		{ID: 2, TargetArtifact: 10},
		{ID: 3}, // artifacts not chosen yet
	}})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got[10]) != 2 || got[10][0] != 1 || got[10][1] != 2 || len(got[11]) != 1 || got[11][0] != 1 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestJobsByArtifactPassesLookupErrors(t *testing.T) {
	if _, err := jobsByArtifact(fakeJobs{err: errors.New("db down")})(context.Background()); err == nil {
		t.Fatal("error swallowed: the cache would treat everything as unused")
	}
}
