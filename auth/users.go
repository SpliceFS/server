package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// ErrNoUser means that no user of that name exists.
var ErrNoUser = errors.New("no such user")

// errEmptyPasswordHash means that a user file has no password_hash.
var errEmptyPasswordHash = errors.New("password_hash is empty")

// allShares is the entry in a user's share list that allows every share.
const allShares = "*"

// maxNameLength is the longest accepted user name.
const maxNameLength = 64

// User is one account read from the users directory.
type User struct {
	// Name is the user name, which is also the file name without ".yaml".
	Name string
	// PasswordHash is the argon2id PHC string.
	PasswordHash string
	// Shares lists the shares the user may use; "*" means every share.
	Shares []string
	// Disabled users cannot log in.
	Disabled bool
}

// Allows reports whether the user may use the named share.
func (u User) Allows(share string) bool {
	return allowsShare(u.Shares, share)
}

// allowsShare reports whether shares lists share or the wildcard.
func allowsShare(shares []string, share string) bool {
	for _, s := range shares {
		if s == allShares || s == share {
			return true
		}
	}
	return false
}

// userFile is the YAML layout of one user file.
type userFile struct {
	PasswordHash string   `yaml:"password_hash"`
	Shares       []string `yaml:"shares"`
	Disabled     bool     `yaml:"disabled"`
}

// Users reads user files from a directory. Files are read on every lookup, so added,
// changed and removed users take effect at once. It is safe for concurrent use.
type Users struct {
	dir string
}

// NewUsers returns a Users that reads "<dir>/<name>.yaml".
func NewUsers(dir string) *Users {
	return &Users{dir: dir}
}

// Lookup reads the named user. A name that is not a valid user name, or has no file,
// returns ErrNoUser. A file that cannot be read or parsed, or has no password hash, is an
// error other than ErrNoUser, because it is an operator mistake that must be visible.
func (u *Users) Lookup(name string) (User, error) {
	if !validName(name) {
		return User{}, ErrNoUser
	}
	data, err := os.ReadFile(filepath.Join(u.dir, name+".yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return User{}, ErrNoUser
	}
	if err != nil {
		return User{}, fmt.Errorf("read user %q: %w", name, err)
	}
	var parsed userFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return User{}, fmt.Errorf("parse user %q: %w", name, err)
	}
	if parsed.PasswordHash == "" {
		return User{}, fmt.Errorf("user %q: %w", name, errEmptyPasswordHash)
	}
	return User{
		Name: name, PasswordHash: parsed.PasswordHash, Shares: parsed.Shares, Disabled: parsed.Disabled,
	}, nil
}

// validName reports whether name matches ^[a-z0-9][a-z0-9._-]{0,63}$. It is the guard
// that keeps a name from escaping the users directory.
func validName(name string) bool {
	if name == "" || len(name) > maxNameLength {
		return false
	}
	for i := range len(name) {
		if !validNameByte(name[i], i == 0) {
			return false
		}
	}
	return true
}

// validNameByte reports whether c may appear in a user name. The separators may not start
// one, so a name cannot be "." or "..".
func validNameByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '-':
		return !first
	default:
		return false
	}
}
