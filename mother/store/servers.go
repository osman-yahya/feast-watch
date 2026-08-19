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
	Arch         string
	AgentVersion string
	LastPush     int64
	// Capabilities is the collector set the agent reported it can actually
	// run. Empty means the agent has not reported yet — which must stay
	// distinct from "supports nothing", since the panel only warns about
	// unsupported collectors once it has heard from the agent.
	Capabilities []string
	// DesiredVersion is the agent version this server should converge on,
	// set per server from the panel. Empty means "leave the agent alone".
	DesiredVersion string
	// UpdateError is the agent's last self-update failure, refreshed on every
	// push so a recovered agent clears it without operator action.
	UpdateError string
	// UninstallRequestedAt is when an operator asked for this agent to be
	// removed from its host, 0 when nobody has. Non-zero means the row is
	// living on borrowed time: the agent is told to remove itself on every
	// push until it reports back (see store/uninstall.go).
	UninstallRequestedAt int64
	// UninstallError is why the agent could not remove itself, refreshed on
	// every push like UpdateError.
	UninstallError string
	CreatedAt      int64
}

// Heartbeat is what one agent push reports about the agent itself, as opposed
// to the metric samples it carries.
type Heartbeat struct {
	AgentVersion string
	Hostname     string
	IP           string
	OS           string
	Arch         string
	UpdateError  string
	// UninstallError is the agent's last removal failure, empty when the last
	// attempt was clean or none was made.
	UninstallError string
}

// nowUnix is the store's clock. A function so tests can reason about it in one
// place rather than reaching for time.Now at each call site.
func nowUnix() int64 { return time.Now().Unix() }

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

const serverCols = `id, name, token, collectors, hostname, ip, os, arch, agent_version,
	last_push, capabilities, desired_version, update_error,
	uninstall_requested_at, uninstall_error, created_at`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var srv Server
	var cols, caps string
	err := row.Scan(&srv.ID, &srv.Name, &srv.Token, &cols, &srv.Hostname, &srv.IP,
		&srv.OS, &srv.Arch, &srv.AgentVersion, &srv.LastPush, &caps,
		&srv.DesiredVersion, &srv.UpdateError,
		&srv.UninstallRequestedAt, &srv.UninstallError, &srv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal([]byte(cols), &srv.Collectors); err != nil {
		return Server{}, fmt.Errorf("decode collectors for server %d: %w", srv.ID, err)
	}
	if err := json.Unmarshal([]byte(caps), &srv.Capabilities); err != nil {
		return Server{}, fmt.Errorf("decode capabilities for server %d: %w", srv.ID, err)
	}
	return srv, nil
}

func (s *Store) ServerByToken(token string) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE token = ?`, token))
}

func (s *Store) ServerByID(id int64) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE id = ?`, id))
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

// TouchServer records a push. Identity fields (hostname/ip/os/arch) ride the
// first push only, so an empty value means "not reported now" and must not
// erase what we already know. UpdateError is the opposite: it rides every
// push, so it is written verbatim and an empty value legitimately clears a
// previously failed update.
func (s *Store) TouchServer(id int64, hb Heartbeat, now int64) error {
	_, err := s.db.Exec(`UPDATE servers SET agent_version = ?, last_push = ?,
		update_error = ?, uninstall_error = ?,
		hostname = CASE WHEN ? != '' THEN ? ELSE hostname END,
		ip       = CASE WHEN ? != '' THEN ? ELSE ip END,
		os       = CASE WHEN ? != '' THEN ? ELSE os END,
		arch     = CASE WHEN ? != '' THEN ? ELSE arch END
		WHERE id = ?`,
		hb.AgentVersion, now, hb.UpdateError, hb.UninstallError,
		hb.Hostname, hb.Hostname, hb.IP, hb.IP,
		hb.OS, hb.OS, hb.Arch, hb.Arch, id)
	return err
}

// SetDesiredVersion targets one server at an agent version. Rollout is
// per-server by design: a single fleet-wide field turns one bad version into
// a fleet-wide outage, with no way to try it on one host first.
func (s *Store) SetDesiredVersion(id int64, version string) error {
	res, err := s.db.Exec(
		`UPDATE servers SET desired_version = ?, update_error = '' WHERE id = ?`, version, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCapabilities records what the agent reported it can run. An empty report
// is ignored rather than stored: the agent sends capabilities on its first
// push only, so every subsequent push carries none and would otherwise erase
// what we already learned.
func (s *Store) SetCapabilities(id int64, capabilities []string) error {
	if len(capabilities) == 0 {
		return nil
	}
	caps, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE servers SET capabilities = ? WHERE id = ?`, string(caps), id)
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
	// servers.id is an INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite
	// may reuse this id for a future server. Purge everything keyed on it, or
	// a fresh server could inherit a deleted server's metrics — or its group
	// memberships, and with them that group's next bulk rollout. No foreign
	// key does this for us: none is enforced on this driver.
	for _, q := range []string{
		`DELETE FROM rollup_1m WHERE server_id = ?`,
		`DELETE FROM rollup_1h WHERE server_id = ?`,
		`DELETE FROM server_group_members WHERE server_id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
