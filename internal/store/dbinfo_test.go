package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Why: About shows the SQLite version and the database size (N13). The size must be the real
// file's, not a constant: it matches the file on disk once the WAL is checkpointed.
func TestDBInfo(t *testing.T) {
	s, path, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	d, err := s.DBInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.SQLiteVersion, "3.") {
		t.Errorf("SQLiteVersion = %q", d.SQLiteVersion)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Size != st.Size() || d.Size == 0 {
		t.Errorf("Size = %d, file has %d bytes", d.Size, st.Size())
	}
}
