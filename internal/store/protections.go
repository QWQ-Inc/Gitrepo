package store

import "context"

// Protection keeps matching branches (or tags) of matching repositories
// from being force-pushed or deleted on the hub.
type Protection struct {
	ID int64
	// Repos is a repository pattern ("acme/*", "*").
	Repos string
	// Ref is a branch name or pattern ("main", "release/*"), or a full ref
	// pattern ("refs/tags/v*").
	Ref        string
	DenyForce  bool
	DenyDelete bool
	CreatedAt  int64
}

func (s *Store) ListProtections(ctx context.Context) ([]*Protection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repos, ref, deny_force, deny_delete, created_at FROM protections ORDER BY repos, ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Protection
	for rows.Next() {
		var p Protection
		var force, del int
		if err := rows.Scan(&p.ID, &p.Repos, &p.Ref, &force, &del, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.DenyForce, p.DenyDelete = force != 0, del != 0
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) CreateProtection(ctx context.Context, p *Protection) error {
	p.CreatedAt = Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO protections (repos, ref, deny_force, deny_delete, created_at) VALUES (?,?,?,?,?)`,
		p.Repos, p.Ref, b2i(p.DenyForce), b2i(p.DenyDelete), p.CreatedAt)
	if err != nil {
		return wrapErr(err)
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

// DeleteProtection removes a rule and returns it (for the audit log).
func (s *Store) DeleteProtection(ctx context.Context, id int64) (*Protection, error) {
	var p Protection
	var force, del int
	err := s.db.QueryRowContext(ctx, `SELECT id, repos, ref, deny_force, deny_delete, created_at FROM protections WHERE id=?`, id).
		Scan(&p.ID, &p.Repos, &p.Ref, &force, &del, &p.CreatedAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	p.DenyForce, p.DenyDelete = force != 0, del != 0
	if _, err := s.db.ExecContext(ctx, `DELETE FROM protections WHERE id=?`, id); err != nil {
		return nil, err
	}
	return &p, nil
}
