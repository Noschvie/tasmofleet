package store

import "context"

// DBInfo describes the database for the About page (design v2, N13).
type DBInfo struct {
	SQLiteVersion string
	Size          int64 // bytes of the main database file (page_count * page_size), WAL not included
}

// DBInfo reads the SQLite library version and the database size; read-only, two pragmas.
func (s *SQLite) DBInfo(ctx context.Context) (DBInfo, error) {
	var d DBInfo
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&d.SQLiteVersion); err != nil {
		return DBInfo{}, err
	}
	var pages, size int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return DBInfo{}, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return DBInfo{}, err
	}
	d.Size = pages * size
	return d, nil
}

// VacuumInto writes a consistent copy of the live database to dst with VACUUM INTO (consistent
// under WAL, while the application keeps writing). dst must not exist; the caller owns its
// directory and permissions.
func (s *SQLite) VacuumInto(ctx context.Context, dst string) error {
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}
