package access

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
)

var ErrForbidden = errors.New("this account does not have access")

type principalKey struct{}

type Principal struct {
	User      string
	Workspace bool
}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func PrincipalFrom(ctx context.Context) Principal {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return Principal{Workspace: true}
	}
	return principal
}

type Membership struct {
	User, Team, Role string
}

type Grant struct {
	Team, API string
	Allowed   bool
}

func (s *Store) CreateUser(ctx context.Context, username string) (string, error) {
	if username == "admin" || !slugPattern.MatchString(username) {
		return "", ErrInput
	}
	secret := newCredential("", "", "").Secret
	digest := sha256.Sum256([]byte(secret))
	err := s.change(ctx, "Portal account created", username, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO portal_users VALUES (?,?) ON CONFLICT DO NOTHING", username, digest[:])
		return changed(result, err, ErrConflict)
	})
	return secret, err
}

func (s *Store) Authenticate(ctx context.Context, username, secret string) (bool, error) {
	if !slugPattern.MatchString(username) || !tokenPattern.MatchString(secret) {
		return false, nil
	}
	var digest []byte
	err := s.db.QueryRowContext(ctx, "SELECT digest FROM portal_users WHERE username=?", username).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(digest, hash[:]) == 1, nil
}

func (s *Store) SetMembership(ctx context.Context, membership Membership) error {
	switch membership.Role {
	case "viewer", "operator", "team-admin", "remove":
	default:
		return ErrInput
	}
	return s.change(ctx, "Team membership changed", membership.Team+"/"+membership.User+" · "+membership.Role, func(tx *sql.Tx) error {
		if membership.Role == "remove" {
			result, err := tx.ExecContext(ctx, "DELETE FROM team_members WHERE username=? AND team=?", membership.User, membership.Team)
			return changed(result, err, ErrMissing)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO team_members SELECT ?,?,?
			WHERE EXISTS(SELECT 1 FROM portal_users WHERE username=?) AND EXISTS(SELECT 1 FROM teams WHERE slug=?)
			ON CONFLICT(username,team) DO UPDATE SET role=excluded.role`,
			membership.User, membership.Team, membership.Role, membership.User, membership.Team)
		return changed(result, err, ErrMissing)
	})
}

func (s *Store) SetGrant(ctx context.Context, grant Grant) error {
	if !slugPattern.MatchString(grant.API) {
		return ErrInput
	}
	action := "Team API access revoked"
	if grant.Allowed {
		action = "Team API access granted"
	}
	return s.change(ctx, action, grant.Team+"/"+grant.API, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO team_grants SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM teams WHERE slug=?)
			ON CONFLICT(team,api) DO UPDATE SET allowed=excluded.allowed`, grant.Team, grant.API, grant.Allowed, grant.Team)
		return changed(result, err, ErrMissing)
	})
}

func (s *Store) Memberships(ctx context.Context) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT username,team,role FROM team_members ORDER BY team,username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var memberships []Membership
	for rows.Next() {
		var item Membership
		if err := rows.Scan(&item.User, &item.Team, &item.Role); err != nil {
			return nil, err
		}
		memberships = append(memberships, item)
	}
	return memberships, rows.Err()
}

func (s *Store) Grants(ctx context.Context) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT team,api,allowed FROM team_grants ORDER BY team,api")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var item Grant
		if err := rows.Scan(&item.Team, &item.API, &item.Allowed); err != nil {
			return nil, err
		}
		grants = append(grants, item)
	}
	return grants, rows.Err()
}

type Permissions struct {
	Workspace bool
	Visible   map[string]bool
	Editable  map[string]bool
	Managed   map[string]map[string]bool
}

func (s *Store) Permissions(ctx context.Context) (Permissions, error) {
	principal := PrincipalFrom(ctx)
	if principal.Workspace {
		return Permissions{Workspace: true}, nil
	}
	p := Permissions{Visible: make(map[string]bool), Editable: make(map[string]bool), Managed: make(map[string]map[string]bool)}
	rows, err := s.db.QueryContext(ctx, `SELECT m.team,m.role,g.api FROM team_members m
		JOIN team_grants g ON g.team=m.team AND g.allowed WHERE m.username=?`, principal.User)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var team, role, api string
		if err := rows.Scan(&team, &role, &api); err != nil {
			return p, err
		}
		p.Visible[api] = true
		if role == "operator" || role == "team-admin" {
			p.Editable[api] = true
		}
		if role == "team-admin" {
			if p.Managed[team] == nil {
				p.Managed[team] = make(map[string]bool)
			}
			p.Managed[team][api] = true
		}
	}
	return p, rows.Err()
}

func canManageKey(ctx context.Context, tx *sql.Tx, team, api string) error {
	principal := PrincipalFrom(ctx)
	if principal.Workspace {
		return nil
	}
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_members m
		JOIN team_grants g ON g.team=m.team AND g.allowed
		WHERE m.username=? AND m.team=? AND m.role='team-admin' AND g.api=?)`, principal.User, team, api).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
