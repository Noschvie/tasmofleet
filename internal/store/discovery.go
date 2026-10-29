package store

import (
	"context"
	"errors"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// ErrProposalLimit is returned by UpsertProposal when the table already holds
// the maximum number of visible proposals and the MAC is not among them.
var ErrProposalLimit = errors.New("store: too many discovery proposals")

// Proposal kinds and sources.
const (
	ProposalNew           = "new"
	ProposalAddressChange = "address-change"
	ProposalConflict      = "conflict"
	ViaMQTT               = "mqtt"
	ViaScan               = "scan"
)

// ProposalRow is a stored discovery proposal. Device carries only what the
// network claimed (name, address, port, MAC, topic, full topic); it never
// holds credentials.
type ProposalRow struct {
	ID         int64
	Kind       string
	Via        string
	Reason     string
	Device     model.Device
	ExistingID model.DeviceID // inventory device the proposal refers to, 0 for "new"
	Dismissed  bool
	FirstSeen  time.Time
	SeenAt     time.Time
}

// DiscoveryStore persists discovery proposals.
type DiscoveryStore interface {
	// UpsertProposal inserts p or refreshes the row of the same MAC. A
	// dismissed row stays dismissed while kind, address, port and existing
	// device are unchanged (a retained MQTT message must not bring it back at
	// every reconnect); any change (also of name, topic or full topic) makes it
	// visible again. maxVisible caps the visible rows of the same class (kind
	// "new" versus the kinds that refer to an existing device); more is
	// ErrProposalLimit for a MAC that has no row yet.
	UpsertProposal(ctx context.Context, p ProposalRow, maxVisible int) (ProposalRow, error)
	// ListProposals returns the visible (not dismissed) rows, newest first.
	ListProposals(ctx context.Context) ([]ProposalRow, error)
	GetProposal(ctx context.Context, id int64) (ProposalRow, error)
	// DismissProposal hides the row but keeps it, see UpsertProposal.
	DismissProposal(ctx context.Context, id int64) error
	// ListDismissedProposals returns the hidden rows, newest first.
	ListDismissedProposals(ctx context.Context) ([]ProposalRow, error)
	// UndismissProposal makes a hidden row visible again.
	UndismissProposal(ctx context.Context, id int64) error
	DeleteProposal(ctx context.Context, id int64) error
	// PruneProposals deletes rows not seen since before and returns the count.
	PruneProposals(ctx context.Context, before time.Time) (int64, error)
}

const proposalCols = "id, mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname"

func scanProposal(r scanner) (ProposalRow, error) {
	var p ProposalRow
	var dismissed int
	var first, seen int64
	var existing int64
	err := r.Scan(&p.ID, &p.Device.MAC, &p.Kind, &p.Via, &p.Reason, &p.Device.Name, &p.Device.Address, &p.Device.Port,
		&p.Device.Topic, &p.Device.FullTopic, &existing, &dismissed, &first, &seen, &p.Device.Net.Hostname)
	p.ExistingID, p.Dismissed = model.DeviceID(existing), dismissed != 0
	p.FirstSeen, p.SeenAt = fromMS(first), fromMS(seen)
	return p, err
}

func (s *SQLite) UpsertProposal(ctx context.Context, p ProposalRow, maxVisible int) (ProposalRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRow{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	now := toMS(s.orNow(p.SeenAt))
	cur, err := scanProposal(tx.QueryRowContext(ctx, "SELECT "+proposalCols+" FROM discovery_proposals WHERE mac=?", p.Device.MAC))
	switch {
	case err == nil:
		same := cur.Kind == p.Kind && cur.Device.Address == p.Device.Address && cur.Device.Port == p.Device.Port && cur.ExistingID == p.ExistingID &&
			cur.Device.Name == p.Device.Name && cur.Device.Topic == p.Device.Topic && cur.Device.FullTopic == p.Device.FullTopic
		dismissed := cur.Dismissed && same
		if _, err := tx.ExecContext(ctx, `UPDATE discovery_proposals SET kind=?, via=?, reason=?, name=?, address=?, port=?, topic=?,
			full_topic=?, existing_id=?, dismissed=?, seen_at=?, hostname=? WHERE id=?`,
			p.Kind, p.Via, p.Reason, p.Device.Name, p.Device.Address, p.Device.Port, p.Device.Topic, p.Device.FullTopic,
			int64(p.ExistingID), b2i(dismissed), now, p.Device.Net.Hostname, cur.ID); err != nil {
			return ProposalRow{}, mapErr(err)
		}
		p.ID, p.FirstSeen, p.Dismissed = cur.ID, cur.FirstSeen, dismissed
	case errors.Is(notFound(err), ErrNotFound):
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM discovery_proposals WHERE dismissed=0 AND (kind='new') = ?",
			p.Kind == ProposalNew).Scan(&n); err != nil {
			return ProposalRow{}, err
		}
		if maxVisible > 0 && n >= maxVisible {
			return ProposalRow{}, ErrProposalLimit
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic,
			full_topic, existing_id, dismissed, first_seen, seen_at, hostname) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?,?)`,
			p.Device.MAC, p.Kind, p.Via, p.Reason, p.Device.Name, p.Device.Address, p.Device.Port, p.Device.Topic,
			p.Device.FullTopic, int64(p.ExistingID), now, now, p.Device.Net.Hostname)
		if err != nil {
			return ProposalRow{}, mapErr(err)
		}
		if p.ID, err = res.LastInsertId(); err != nil {
			return ProposalRow{}, err
		}
		p.FirstSeen, p.Dismissed = fromMS(now), false
	default:
		return ProposalRow{}, err
	}
	p.SeenAt = fromMS(now)
	return p, tx.Commit()
}

func (s *SQLite) ListProposals(ctx context.Context) ([]ProposalRow, error) {
	return s.listProposals(ctx, 0)
}

func (s *SQLite) ListDismissedProposals(ctx context.Context) ([]ProposalRow, error) {
	return s.listProposals(ctx, 1)
}

func (s *SQLite) listProposals(ctx context.Context, dismissed int) ([]ProposalRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+proposalCols+" FROM discovery_proposals WHERE dismissed=? ORDER BY seen_at DESC, id DESC", dismissed)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []ProposalRow
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) GetProposal(ctx context.Context, id int64) (ProposalRow, error) {
	p, err := scanProposal(s.db.QueryRowContext(ctx, "SELECT "+proposalCols+" FROM discovery_proposals WHERE id=?", id))
	return p, notFound(err)
}

func (s *SQLite) DismissProposal(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, "UPDATE discovery_proposals SET dismissed=1 WHERE id=?", id))
}

func (s *SQLite) UndismissProposal(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, "UPDATE discovery_proposals SET dismissed=0 WHERE id=?", id))
}

func (s *SQLite) DeleteProposal(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM discovery_proposals WHERE id=?", id))
}

func (s *SQLite) PruneProposals(ctx context.Context, before time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM discovery_proposals WHERE seen_at < ?", toMS(before)))
}
