package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidGroupName is returned when a group name is empty or unusable.
var ErrInvalidGroupName = errors.New("invalid group name: 1-64 characters, no control characters")

// ErrDuplicateGroupName is returned when the name is already taken. It is
// distinguished from a storage failure so the API can answer 409 rather than
// 500 — a name collision is the caller's to fix.
var ErrDuplicateGroupName = errors.New("group name already exists")

const maxGroupNameRunes = 64

// Group is a named set of servers, used to filter the fleet and to target a
// whole set at an agent version at once.
type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// ServerCount is filled by ListGroups; membership itself is read
	// separately, since the list view needs counts and only the detail views
	// need the members.
	ServerCount int   `json:"server_count"`
	CreatedAt   int64 `json:"created_at"`
}

// validGroupName trims and bounds the name.
//
// Deliberately broader than validName for servers: a server name is rendered
// raw into the generated install shell script, while a group name never leaves
// JSON. Turkish names like "Veritabanı Sunucuları" have to work, so the rule
// is about length and control characters rather than an ASCII charset.
func validGroupName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maxGroupNameRunes {
		return "", ErrInvalidGroupName
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", ErrInvalidGroupName
		}
	}
	return trimmed, nil
}

func (s *Store) CreateGroup(name string) (Group, error) {
	clean, err := validGroupName(name)
	if err != nil {
		return Group{}, err
	}
	g := Group{Name: clean, CreatedAt: time.Now().Unix()}
	res, err := s.db.Exec(
		`INSERT INTO server_groups (name, created_at) VALUES (?,?)`, g.Name, g.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Group{}, ErrDuplicateGroupName
		}
		return Group{}, err
	}
	g.ID, _ = res.LastInsertId()
	return g, nil
}

// ListGroups returns every group with how many servers it holds.
func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query(`
		SELECT g.id, g.name, g.created_at, COUNT(m.server_id)
		FROM server_groups g
		LEFT JOIN server_group_members m ON m.group_id = g.id
		GROUP BY g.id, g.name, g.created_at
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.ServerCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) RenameGroup(id int64, name string) error {
	clean, err := validGroupName(name)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE server_groups SET name = ? WHERE id = ?`, clean, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicateGroupName
		}
		return err
	}
	return requireOneRow(res)
}

// DeleteGroup removes the group and its memberships, never its servers.
func (s *Store) DeleteGroup(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM server_group_members WHERE group_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM server_groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := requireOneRow(res); err != nil {
		return err
	}
	return tx.Commit()
}

// SetGroupServers replaces the group's membership wholesale. Replace rather
// than merge: the caller sends the intended set, so "remove the last member"
// is expressible and a lost update cannot silently re-add someone.
func (s *Store) SetGroupServers(groupID int64, serverIDs []int64) error {
	if err := s.groupExists(groupID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM server_group_members WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	if len(serverIDs) > 0 {
		values := make([]string, 0, len(serverIDs))
		args := make([]any, 0, len(serverIDs)*2)
		for _, id := range serverIDs {
			values = append(values, "(?,?)")
			args = append(args, groupID, id)
		}
		// Unknown server ids are ignored rather than rejected: a concurrent
		// delete would otherwise fail the whole call, and a membership row
		// pointing at nothing is invisible to every read (they all join).
		query := `INSERT OR IGNORE INTO server_group_members (group_id, server_id) VALUES ` +
			strings.Join(values, ",")
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("set group members: %w", err)
		}
	}
	return tx.Commit()
}

// ServersInGroup lists the group's members, in the same shape as ListServers.
func (s *Store) ServersInGroup(groupID int64) ([]Server, error) {
	rows, err := s.db.Query(`SELECT `+prefixed(serverCols, "s")+`
		FROM servers s
		JOIN server_group_members m ON m.server_id = s.id
		WHERE m.group_id = ?
		ORDER BY s.name`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Server
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// GroupsByServer maps every server id to the groups it belongs to, so the
// server list attaches them with one query instead of one per row.
func (s *Store) GroupsByServer() (map[int64][]Group, error) {
	rows, err := s.db.Query(`
		SELECT m.server_id, g.id, g.name, g.created_at
		FROM server_group_members m
		JOIN server_groups g ON g.id = m.group_id
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]Group{}
	for rows.Next() {
		var serverID int64
		var g Group
		if err := rows.Scan(&serverID, &g.ID, &g.Name, &g.CreatedAt); err != nil {
			return nil, err
		}
		out[serverID] = append(out[serverID], g)
	}
	return out, rows.Err()
}

func (s *Store) groupExists(id int64) error {
	var found int64
	err := s.db.QueryRow(`SELECT id FROM server_groups WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// prefixed qualifies a comma-separated column list with a table alias, so the
// shared serverCols can be reused in a join without ambiguity.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
