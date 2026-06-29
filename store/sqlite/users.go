package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// DefaultUserName is the seeded user every catalog starts with, so single-user
// setups need no user management.
const DefaultUserName = "default"

// ensureDefaultUser seeds the default user when the catalog has none. It runs on
// a read-write Open (after migration), so a fresh catalog always has a usable
// playback identity. pids are ULIDs, so this is done in Go rather than in the
// migration SQL.
func (s *Store) ensureDefaultUser(ctx context.Context) error {
	const op = "store.ensureDefaultUser"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user").Scan(&n); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n > 0 {
			return nil
		}
		pid := model.NewPID()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user(pid, name, is_default, created_at) VALUES (?,?,1,?)",
			string(pid), DefaultUserName, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "user", pid, model.OpCreate)
	})
}

// CreateUser adds a playback user. Names are unique; a duplicate is CodeConflict.
func (s *Store) CreateUser(ctx context.Context, name string) (*model.User, error) {
	const op = "store.CreateUser"
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "user name is required")
	}
	var u *model.User
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		pid := model.NewPID()
		_, err := tx.ExecContext(ctx,
			"INSERT INTO user(pid, name, is_default, created_at) VALUES (?,?,0,?)", string(pid), name, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return waxerr.New(waxerr.CodeConflict, op, "user already exists: "+name)
			}
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		u = &model.User{PID: pid, Name: name, CreatedAt: now}
		return appendChange(ctx, tx, "user", pid, model.OpCreate)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Users lists all users, the default first then by name.
func (s *Store) Users(ctx context.Context) ([]*model.User, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT id, pid, name, is_default, created_at FROM user ORDER BY is_default DESC, name")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.Users", err)
	}
	defer rows.Close()
	var out []*model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.Users", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DefaultUser returns the seeded default user.
func (s *Store) DefaultUser(ctx context.Context) (*model.User, error) {
	u, err := scanUser(s.read.QueryRowContext(ctx,
		"SELECT id, pid, name, is_default, created_at FROM user WHERE is_default = 1 LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.DefaultUser", "no default user")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.DefaultUser", err)
	}
	return u, nil
}

// userIDByPID resolves a user pid (empty means the default user) to its rowid.
func userIDByPID(ctx context.Context, q queryer, userPID model.PID, op string) (int64, error) {
	var id int64
	var err error
	if userPID == "" {
		err = q.QueryRowContext(ctx, "SELECT id FROM user WHERE is_default = 1 LIMIT 1").Scan(&id)
	} else {
		err = q.QueryRowContext(ctx, "SELECT id FROM user WHERE pid = ?", string(userPID)).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such user")
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}

func scanUser(sc rowScanner) (*model.User, error) {
	var u model.User
	var isDefault int
	if err := sc.Scan(&u.ID, &u.PID, &u.Name, &isDefault, &u.CreatedAt); err != nil {
		return nil, err
	}
	u.IsDefault = isDefault == 1
	return &u, nil
}
