package backup

import "context"

// Report is what one backup run measured about how it travelled, for the callers that word the
// result (Note): the transport of the dump and its status snapshot, and the transport of the files
// archive when one was stored. It is the same measurement the audit entry of the run keeps ("via"),
// handed out so no caller works the transports out again from a trace that covers the whole run:
// that trace reads "http+mqtt" for a dump over MQTT whose files came over HTTP, and says nothing
// about which part used which (#926).
type Report struct {
	Dump  string // "mqtt" or "http"; "" until a backup ran through this context
	Files string // "http" (the files cannot come over MQTT); "" when no files archive was stored
}

type reportKey struct{}

// WithReport returns ctx carrying a Report that the next Backup fills in, and the Report. The caller
// reads it after Backup returned without an error; after an error it holds nothing.
func WithReport(ctx context.Context) (context.Context, *Report) {
	r := &Report{}
	return context.WithValue(ctx, reportKey{}, r), r
}

// Record fills the Report that WithReport put in ctx; without one it does nothing (a backup taken
// by a restore, a replacement or before an update has no caller that words it). A Backup calls it
// once, in the goroutine that called Backup, before it returns.
func Record(ctx context.Context, dump, files string) {
	if r, _ := ctx.Value(reportKey{}).(*Report); r != nil {
		r.Dump, r.Files = dump, files
	}
}
