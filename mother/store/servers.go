package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var DefaultCollectors = []string{"cpu", "memory", "uptime", "disk"}

// validName restricts server names to a charset safe for direct interpolation
// into the generated install shell script (see install.sh.tmpl).
var validName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type Server struct {
	ID           int64
	Name         string
	Token        string
	Collectors   []string
	Hostname     string
	IP           string
	OS           string
	AgentVersion string
	LastPush     int64
	CreatedAt    int64
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "tk_" + hex.EncodeToString(b)
}

func (s *Store) AddServer(name string) (Server, error) {
	if !validName.MatchString(name) {
		return Server{}, ErrInvalidName
	}
	srv := Server{
		Name: name, Token: newToken(),
		Collectors: DefaultCollectors, CreatedAt: time.Now().Unix(),
	}
	cols, _ := json.Marshal(srv.Collectors)
	res, err := s.db.Exec(
		`INSERT INTO servers (name, token, collectors, created_at) VALUES (?,?,?,?)`,
		srv.Name, srv.Token, string(cols), srv.CreatedAt)
	if err != nil {
		return Server{}, err
	}
	srv.ID, _ = res.LastInsertId()
	return srv, nil
}

const serverCols = `id, name, token, collectors, hostname, ip, os, agent_version, last_push, created_at`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var srv Server
	var cols string
	err := row.Scan(&srv.ID, &srv.Name, &srv.Token, &cols, &srv.Hostname, &srv.IP,
		&srv.OS, &srv.AgentVersion, &srv.LastPush, &srv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal([]byte(cols), &srv.Collectors); err != nil {
		return Server{}, fmt.Errorf("decode collectors for server %d: %w", srv.ID, err)
	}
	return srv, nil
}

func (s *Store) ServerByToken(token string) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE token = ?`, token))
}

func (s *Store) ServerByName(name string) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE name = ?`, name))
}

func (s *Store) ListServers() ([]Server, error) {
	rows, err := s.db.Query(`SELECT ` + serverCols + ` FROM servers ORDER BY name`)
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

func (s *Store) TouchServer(id int64, agentVersion, hostname, ip, osName string, now int64) error {
	_, err := s.db.Exec(`UPDATE servers SET agent_version = ?, last_push = ?,
		hostname = CASE WHEN ? != '' THEN ? ELSE hostname END,
		ip       = CASE WHEN ? != '' THEN ? ELSE ip END,
		os       = CASE WHEN ? != '' THEN ? ELSE os END
		WHERE id = ?`,
		agentVersion, now, hostname, hostname, ip, ip, osName, osName, id)
	return err
}

func (s *Store) SetCollectors(id int64, collectors []string) error {
	cols, _ := json.Marshal(collectors)
	_, err := s.db.Exec(`UPDATE servers SET collectors = ? WHERE id = ?`, string(cols), id)
	return err
}

func (s *Store) DeleteServer(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM servers WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM samples WHERE server_id = ?`, id); err != nil {
		return err
	}
	// servers.id is an INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite
	// may reuse this id for a future server. Purge rollup history too, or a
	// fresh server could inherit a deleted server's metrics.
	if _, err := tx.Exec(`DELETE FROM rollup_1m WHERE server_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM rollup_1h WHERE server_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
