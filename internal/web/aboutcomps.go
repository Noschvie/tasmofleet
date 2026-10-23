package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// DBInfoer reports the database version and size (store.SQLite); About's database row (N13).
type DBInfoer interface {
	DBInfo(ctx context.Context) (store.DBInfo, error)
}

// aboutComps is the read-only health summary of About (design v2, N13): MQTT, backup decoder,
// release feed, database and background jobs, in that order. Rows of services this installation
// does not have are left out. Every row comes from state the services already hold; nothing
// here contacts a device or upstream.
func (h *handler) aboutComps(r *http.Request) []aboutComp {
	comps := []aboutComp{mqttComp(h.d.MQTT()), decoderComp(h.d.DecoderVersion)}
	now := h.d.Clock.Now()
	if sr, ok := h.d.Firmware.(firmware.StatusReporter); ok {
		comps = append(comps, feedComp(sr.IndexStatus(), now))
	}
	if h.d.Database != nil {
		info, err := h.d.Database.DBInfo(r.Context())
		if err != nil {
			h.log.WarnContext(r.Context(), "about: database info", "err", err)
		}
		comps = append(comps, dbComp(info, err))
	}
	if h.d.Jobs != nil {
		jobs, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{States: activeJobStates})
		if err != nil {
			h.log.WarnContext(r.Context(), "about: active jobs", "err", err)
		}
		comps = append(comps, jobsComp(jobs, err))
	}
	return comps
}

// feedComp is the release feed row. The error text of a failed fetch is not shown: it can carry
// a source URL, and the firmware page is where an admin reads it.
func feedComp(st firmware.IndexStatus, now time.Time) aboutComp {
	c := aboutComp{Name: "Release feed"}
	switch {
	case st.LastAttempt.IsZero():
		c.Detail, c.State, c.Tone = "Not checked yet", "Waiting", "neutral"
	case st.LastError != "" && !st.Loaded:
		c.Detail, c.State, c.Tone = "Last check "+agoWords(now, st.LastAttempt)+" failed · no release list yet", "Failing", "danger"
	case st.LastError != "":
		c.Detail = "Last check " + agoWords(now, st.LastAttempt) + " failed · using the list from " + agoWords(now, st.LastSuccess)
		c.State, c.Tone = "Stale", "warn"
	default:
		c.Detail, c.State, c.Tone = "Checked "+agoWords(now, st.LastSuccess), "Healthy", "ok"
	}
	return c
}

// dbComp is the database row: SQLite version and file size.
func dbComp(d store.DBInfo, err error) aboutComp {
	if err != nil {
		return aboutComp{Name: "Database", Detail: "Not readable · see the server log", State: "Failing", Tone: "danger"}
	}
	return aboutComp{Name: "Database", Detail: "SQLite " + d.SQLiteVersion + " · " + humanBytes(d.Size), State: "Healthy", Tone: "ok"}
}

// jobsComp is the background jobs row: running and queued update and policy jobs.
func jobsComp(jobs []model.Job, err error) aboutComp {
	if err != nil {
		return aboutComp{Name: "Background jobs", Detail: "Not readable · see the server log", State: "Failing", Tone: "danger"}
	}
	queued := 0
	for _, j := range jobs {
		if j.State == model.JobQueued {
			queued++
		}
	}
	running := len(jobs) - queued
	if running == 0 && queued == 0 {
		return aboutComp{Name: "Background jobs", Detail: "Idle", State: "Healthy", Tone: "ok"}
	}
	var parts []string
	if running > 0 {
		parts = append(parts, plural(running, "job")+" running")
	}
	if queued > 0 {
		parts = append(parts, plural(queued, "job")+" queued")
	}
	return aboutComp{Name: "Background jobs", Detail: strings.Join(parts, " · "), State: "Busy", Tone: "ok"}
}
