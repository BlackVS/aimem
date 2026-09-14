// Package access owns hub-local users, access groups, project grants and ordinary
// tokens. Knowledge groups and host-console admin tokens remain separate.
package access

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aimem/internal/uuidv7"
	_ "modernc.org/sqlite"
)

var ErrDenied = errors.New("access denied")

type Store struct{ db *sql.DB }

// Open creates only the hub access store, never a project. The caller must own
// the state root. No schema/version change is made to existing project databases.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "access.db")
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("access database is a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	db, err := sql.Open("sqlite", u.String()+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// OpenExisting is for authentication: an unauthenticated request must not create
// an access store on a hub that has never enabled ordinary credentials.
func OpenExisting(root string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(root, "access.db")); err != nil {
		return nil, err
	}
	return Open(root)
}

func (s *Store) migrate() error {
	var current int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current == 1 {
		return nil
	}
	if current > 1 {
		return fmt.Errorf("access schema %d is newer than supported schema 1", current)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("access schema %d is newer than supported schema 1", version)
	}
	if version == 0 {
		_, err = tx.Exec(`
CREATE TABLE users(id TEXT PRIMARY KEY, name TEXT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0);
CREATE TABLE access_groups(id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE members(group_id TEXT NOT NULL REFERENCES access_groups(id), user_id TEXT NOT NULL REFERENCES users(id), PRIMARY KEY(group_id,user_id));
CREATE TABLE grants(project TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('user','group')), subject TEXT NOT NULL, PRIMARY KEY(project,kind,subject));
CREATE TABLE tokens(id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), label TEXT NOT NULL, project TEXT NOT NULL, digest TEXT NOT NULL UNIQUE, expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE audit(id TEXT PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, subject TEXT NOT NULL);
PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Member struct {
	GroupID string `json:"group_id"`
	UserID  string `json:"user_id"`
}
type Grant struct {
	Project string `json:"project_instance"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}
type Token struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Label     string    `json:"label"`
	Project   string    `json:"project_instance,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}
type Event struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Subject string `json:"subject"`
}
type Snapshot struct {
	Users   []User   `json:"users"`
	Groups  []Group  `json:"groups"`
	Members []Member `json:"members"`
	Grants  []Grant  `json:"grants"`
	Tokens  []Token  `json:"tokens"`
	Audit   []Event  `json:"recent_audit"`
}

func validName(name string) error {
	if strings.TrimSpace(name) != name || name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n\x00") {
		return fmt.Errorf("name must be 1-128 UTF-8 bytes without surrounding whitespace or control lines")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name contains a control character")
		}
	}
	return nil
}

func audit(tx *sql.Tx, actor, action, subject string) error {
	if actor == "" {
		return fmt.Errorf("authenticated actor is required")
	}
	_, err := tx.Exec("INSERT INTO audit VALUES(?,?,?,?,?)", uuidv7.New(), time.Now().UTC().Format(time.RFC3339Nano), actor, action, subject)
	return err
}
func (s *Store) change(actor, action, subject string, fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	if err = audit(tx, actor, action, subject); err != nil {
		return err
	}
	return tx.Commit()
}
func requireRow(tx *sql.Tx, table, id string) error {
	var n int
	// table is selected by these methods, never supplied as SQL by callers.
	if err := tx.QueryRow("SELECT count(*) FROM "+table+" WHERE id=?", id).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("unknown %s ID", table)
	}
	return nil
}

func (s *Store) CreateUser(actor, name string) (User, error) {
	if err := validName(name); err != nil {
		return User{}, err
	}
	u := User{ID: uuidv7.New(), Name: name}
	err := s.change(actor, "user.create", u.ID, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO users(id,name) VALUES(?,?)", u.ID, u.Name)
		return err
	})
	return u, err
}
func (s *Store) SetUser(actor, id, name string, disabled bool) error {
	if err := validName(name); err != nil {
		return err
	}
	return s.change(actor, "user.update", id, func(tx *sql.Tx) error {
		if err := requireRow(tx, "users", id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE users SET name=?,disabled=? WHERE id=?", name, disabled, id)
		return err
	})
}
func (s *Store) CreateGroup(actor, name string) (Group, error) {
	if err := validName(name); err != nil {
		return Group{}, err
	}
	g := Group{ID: uuidv7.New(), Name: name}
	err := s.change(actor, "group.create", g.ID, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO access_groups VALUES(?,?)", g.ID, g.Name)
		return err
	})
	return g, err
}
func (s *Store) SetMember(actor, group, user string, present bool) error {
	return s.change(actor, fmt.Sprintf("member.%t", present), group+"/"+user, func(tx *sql.Tx) error {
		if err := requireRow(tx, "access_groups", group); err != nil {
			return err
		}
		if err := requireRow(tx, "users", user); err != nil {
			return err
		}
		query := "DELETE FROM members WHERE group_id=? AND user_id=?"
		if present {
			query = "INSERT OR IGNORE INTO members VALUES(?,?)"
		}
		_, err := tx.Exec(query, group, user)
		return err
	})
}
func (s *Store) SetGrant(actor, project, kind, subject string, present bool) error {
	if project == "" {
		return fmt.Errorf("project instance is required")
	}
	table := "users"
	if kind == "group" {
		table = "access_groups"
	} else if kind != "user" {
		return fmt.Errorf("kind must be user or group")
	}
	return s.change(actor, fmt.Sprintf("grant.%t", present), project+"/"+kind+"/"+subject, func(tx *sql.Tx) error {
		if err := requireRow(tx, table, subject); err != nil {
			return err
		}
		query := "DELETE FROM grants WHERE project=? AND kind=? AND subject=?"
		if present {
			query = "INSERT OR IGNORE INTO grants VALUES(?,?,?)"
		}
		_, err := tx.Exec(query, project, kind, subject)
		return err
	})
}

type rowQuery interface{ QueryRow(string, ...any) *sql.Row }

// CanWrite reports whether user is enabled and holds a current direct or
// group grant on project (an access instance id). It reads current state
// on every call; callers re-check per attempt and never cache the answer.
func (s *Store) CanWrite(user, project string) (bool, error) { return canWrite(s.db, user, project) }

// IdentityExists reports whether kind ("user" or "group") and id name a
// known identity, disabled or not: an assignment must point at something
// real; whether it is still active is the reader's concern.
func (s *Store) IdentityExists(kind, id string) (bool, error) {
	table := map[string]string{"user": "users", "group": "access_groups"}[kind]
	if table == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE id=?", id).Scan(&n)
	return n == 1, err
}

func canWrite(q rowQuery, user, project string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT count(*) FROM users u WHERE u.id=? AND u.disabled=0 AND EXISTS(
SELECT 1 FROM grants g WHERE g.project=? AND ((g.kind='user' AND g.subject=u.id) OR
(g.kind='group' AND EXISTS(SELECT 1 FROM members m WHERE m.group_id=g.subject AND m.user_id=u.id))))`, user, project).Scan(&n)
	return n == 1, err
}

// Issue never accepts a role or an admin capability. Its return value is the
// only place the secret appears; snapshot/audit contain no digest or secret.
func (s *Store) Issue(actor, user, label, project string, expires time.Time) (Token, string, error) {
	if err := validName(label); err != nil {
		return Token{}, "", err
	}
	if !expires.After(time.Now()) || expires.After(time.Now().Add(366*24*time.Hour)) {
		return Token{}, "", fmt.Errorf("expiry must be in the future and within 366 days")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Token{}, "", err
	}
	secret := "aimem_user_" + hex.EncodeToString(random[:])
	sum := sha256.Sum256([]byte(secret))
	t := Token{ID: uuidv7.New(), UserID: user, Label: label, Project: project, ExpiresAt: expires.UTC().Truncate(time.Second)}
	err := s.change(actor, "token.issue", t.ID, func(tx *sql.Tx) error {
		var disabled bool
		if err := tx.QueryRow("SELECT disabled FROM users WHERE id=?", user).Scan(&disabled); err != nil {
			return fmt.Errorf("unknown user: %w", err)
		}
		if disabled {
			return ErrDenied
		}
		if project != "" {
			ok, err := canWrite(tx, user, project)
			if err != nil {
				return err
			}
			if !ok {
				return ErrDenied
			}
		}
		_, err := tx.Exec("INSERT INTO tokens(id,user_id,label,project,digest,expires_at) VALUES(?,?,?,?,?,?)", t.ID, user, label, project, hex.EncodeToString(sum[:]), t.ExpiresAt.Unix())
		return err
	})
	if err != nil {
		return Token{}, "", err
	}
	return t, secret, nil
}
func (s *Store) Revoke(actor, id string) error {
	return s.change(actor, "token.revoke", id, func(tx *sql.Tx) error {
		if err := requireRow(tx, "tokens", id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE tokens SET revoked=1 WHERE id=?", id)
		return err
	})
}

type Identity struct {
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
	Name    string `json:"name"`
	Project string `json:"project_instance,omitempty"`
}

func (s *Store) Authenticate(secret string) (Identity, error) {
	sum := sha256.Sum256([]byte(secret))
	var id Identity
	err := s.db.QueryRow(`SELECT u.id,t.id,u.name,t.project FROM tokens t JOIN users u ON u.id=t.user_id
WHERE t.digest=? AND t.revoked=0 AND t.expires_at>? AND u.disabled=0`, hex.EncodeToString(sum[:]), time.Now().Unix()).Scan(&id.UserID, &id.TokenID, &id.Name, &id.Project)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrDenied
	}
	return id, err
}

// Authorize rechecks token and membership; it never trusts a client-supplied
// user or a previously cached identity. Empty project denotes a read-only check.
func (s *Store) Authorize(secret, project string) (Identity, error) {
	id, err := s.Authenticate(secret)
	if err != nil {
		return Identity{}, err
	}
	if project != "" {
		if id.Project != project {
			return Identity{}, ErrDenied
		}
		ok, err := canWrite(s.db, id.UserID, project)
		if err != nil {
			return Identity{}, err
		}
		if !ok {
			return Identity{}, ErrDenied
		}
	}
	return id, nil
}

// Snapshot is an administrator-only view. Audit is bounded; credentials remain
// usable regardless of whether their metadata has been listed.
func (s *Store) Snapshot() (Snapshot, error) {
	out := Snapshot{Users: []User{}, Groups: []Group{}, Members: []Member{}, Grants: []Grant{}, Tokens: []Token{}, Audit: []Event{}}
	queries := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{"SELECT id,name,disabled FROM users ORDER BY id", func(r *sql.Rows) error {
			var v User
			err := r.Scan(&v.ID, &v.Name, &v.Disabled)
			out.Users = append(out.Users, v)
			return err
		}},
		{"SELECT id,name FROM access_groups ORDER BY id", func(r *sql.Rows) error {
			var v Group
			err := r.Scan(&v.ID, &v.Name)
			out.Groups = append(out.Groups, v)
			return err
		}},
		{"SELECT group_id,user_id FROM members ORDER BY group_id,user_id", func(r *sql.Rows) error {
			var v Member
			err := r.Scan(&v.GroupID, &v.UserID)
			out.Members = append(out.Members, v)
			return err
		}},
		{"SELECT project,kind,subject FROM grants ORDER BY project,kind,subject", func(r *sql.Rows) error {
			var v Grant
			err := r.Scan(&v.Project, &v.Kind, &v.Subject)
			out.Grants = append(out.Grants, v)
			return err
		}},
		{"SELECT id,user_id,label,project,expires_at,revoked FROM tokens ORDER BY id", func(r *sql.Rows) error {
			var v Token
			var expiry int64
			err := r.Scan(&v.ID, &v.UserID, &v.Label, &v.Project, &expiry, &v.Revoked)
			v.ExpiresAt = time.Unix(expiry, 0).UTC()
			out.Tokens = append(out.Tokens, v)
			return err
		}},
		{"SELECT id,at,actor,action,subject FROM audit ORDER BY id DESC LIMIT 100", func(r *sql.Rows) error {
			var v Event
			err := r.Scan(&v.ID, &v.At, &v.Actor, &v.Action, &v.Subject)
			out.Audit = append(out.Audit, v)
			return err
		}},
	}
	for _, q := range queries {
		rows, err := s.db.Query(q.query)
		if err != nil {
			return Snapshot{}, err
		}
		for rows.Next() {
			if err = q.scan(rows); err != nil {
				rows.Close()
				return Snapshot{}, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Snapshot{}, err
		}
	}
	return out, nil
}
