package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a sql.DB with helper methods.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite performs best with a single writer connection.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetConnMaxLifetime(0)

	// Enable WAL mode and foreign keys.
	if _, err := sqlDB.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		return nil, fmt.Errorf("pragma: %w", err)
	}

	if err := migrate(sqlDB); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{sqlDB}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// ---- Host ----

type Host struct {
	ID        string
	APIKey    string
	Name      string
	CreatedAt time.Time
}

func (d *DB) CreateHost(h *Host) error {
	_, err := d.Exec(
		`INSERT INTO hosts(id, api_key, name, created_at) VALUES(?,?,?,?)`,
		h.ID, h.APIKey, h.Name, h.CreatedAt,
	)
	return err
}

func (d *DB) GetHostByAPIKey(key string) (*Host, error) {
	row := d.QueryRow(`SELECT id, api_key, name, created_at FROM hosts WHERE api_key=?`, key)
	h := &Host{}
	if err := row.Scan(&h.ID, &h.APIKey, &h.Name, &h.CreatedAt); err != nil {
		return nil, err
	}
	return h, nil
}

// ---- Room ----

type Room struct {
	ID             string
	HostID         string
	Name           string
	SessionSeconds int
	Online         bool
	CurrentViewer  *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (d *DB) CreateRoom(r *Room) error {
	_, err := d.Exec(
		`INSERT INTO rooms(id, host_id, name, session_seconds, online, current_viewer, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.HostID, r.Name, r.SessionSeconds,
		boolToInt(r.Online), r.CurrentViewer, r.CreatedAt, r.UpdatedAt,
	)
	return err
}

func (d *DB) GetRoom(id string) (*Room, error) {
	row := d.QueryRow(
		`SELECT id, host_id, name, session_seconds, online, current_viewer, created_at, updated_at
		 FROM rooms WHERE id=?`, id,
	)
	return scanRoom(row)
}

func (d *DB) GetRoomByIDAndHostID(id, hostID string) (*Room, error) {
	row := d.QueryRow(
		`SELECT id, host_id, name, session_seconds, online, current_viewer, created_at, updated_at
		 FROM rooms WHERE id=? AND host_id=?`, id, hostID,
	)
	return scanRoom(row)
}

func (d *DB) UpdateRoomHeartbeat(id string, online bool, currentViewer *string) error {
	_, err := d.Exec(
		`UPDATE rooms SET online=?, current_viewer=?, updated_at=datetime('now') WHERE id=?`,
		boolToInt(online), currentViewer, id,
	)
	return err
}

func scanRoom(row *sql.Row) (*Room, error) {
	r := &Room{}
	var onlineInt int
	if err := row.Scan(
		&r.ID, &r.HostID, &r.Name, &r.SessionSeconds,
		&onlineInt, &r.CurrentViewer, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	r.Online = onlineInt != 0
	return r, nil
}

// ---- Token ----

type Token struct {
	Code       string
	RoomID     string
	HostID     string
	Redeemed   bool
	Revoked    bool
	ViewerID   *string
	CreatedAt  time.Time
	RedeemedAt *time.Time
}

func (d *DB) CreateToken(t *Token) error {
	_, err := d.Exec(
		`INSERT INTO tokens(code, room_id, host_id, redeemed, revoked, created_at)
		 VALUES(?,?,?,0,0,?)`,
		t.Code, t.RoomID, t.HostID, t.CreatedAt,
	)
	return err
}

func (d *DB) GetToken(code string) (*Token, error) {
	row := d.QueryRow(
		`SELECT code, room_id, host_id, redeemed, revoked, viewer_id, created_at, redeemed_at
		 FROM tokens WHERE code=?`, code,
	)
	t := &Token{}
	var redeemedInt, revokedInt int
	if err := row.Scan(
		&t.Code, &t.RoomID, &t.HostID,
		&redeemedInt, &revokedInt, &t.ViewerID,
		&t.CreatedAt, &t.RedeemedAt,
	); err != nil {
		return nil, err
	}
	t.Redeemed = redeemedInt != 0
	t.Revoked = revokedInt != 0
	return t, nil
}

func (d *DB) RedeemToken(code, viewerID string) error {
	_, err := d.Exec(
		`UPDATE tokens SET redeemed=1, viewer_id=?, redeemed_at=datetime('now')
		 WHERE code=? AND redeemed=0 AND revoked=0`,
		viewerID, code,
	)
	return err
}

func (d *DB) RevokeToken(code string) error {
	_, err := d.Exec(
		`UPDATE tokens SET revoked=1 WHERE code=? AND redeemed=0`,
		code,
	)
	return err
}

// GetTokenByCodeAndHostID returns a token if it belongs to the given host.
func (d *DB) GetTokenByCodeAndHostID(code, hostID string) (*Token, error) {
	row := d.QueryRow(
		`SELECT code, room_id, host_id, redeemed, revoked, viewer_id, created_at, redeemed_at
		 FROM tokens WHERE code=? AND host_id=?`, code, hostID,
	)
	t := &Token{}
	var redeemedInt, revokedInt int
	if err := row.Scan(
		&t.Code, &t.RoomID, &t.HostID,
		&redeemedInt, &revokedInt, &t.ViewerID,
		&t.CreatedAt, &t.RedeemedAt,
	); err != nil {
		return nil, err
	}
	t.Redeemed = redeemedInt != 0
	t.Revoked = revokedInt != 0
	return t, nil
}

// ---- Session ----

type Session struct {
	ID        string
	RoomID    string
	ViewerID  string
	TokenCode string
	JWT       string
	QueuePos  int
	Active    bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (d *DB) CreateSession(s *Session) error {
	_, err := d.Exec(
		`INSERT INTO sessions(id, room_id, viewer_id, token_code, jwt, queue_pos, active, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		s.ID, s.RoomID, s.ViewerID, s.TokenCode, s.JWT,
		s.QueuePos, boolToInt(s.Active), s.CreatedAt, s.ExpiresAt,
	)
	return err
}

// CountActiveSessionsInRoom counts sessions not yet expired.
func (d *DB) CountActiveSessionsInRoom(roomID string) (int, error) {
	row := d.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE room_id=? AND active=1 AND expires_at > datetime('now')`,
		roomID,
	)
	var n int
	return n, row.Scan(&n)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
