package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// Problems with a user file. They only reach the log. The text of one never contains the content
// of the file, because a mistake in the file may put a password hash where a key or a value
// belongs. The YAML decoder quotes values and key names in its errors, so none of its text is
// passed on except the line number.
var (
	errNotUserFile = errors.New(
		"user files are named by the user id since this version; run splicefsdctl user add to make one")
	errEmptyName         = errors.New("name is empty")
	errBadName           = errors.New("name must match ^[a-z0-9][a-z0-9._-]{0,63}$")
	errEmptyPasswordHash = errors.New("password_hash is empty")
	errUserFileEmpty     = errors.New("the file is empty")
	errUserNotYAML       = errors.New("not valid YAML")
	errUserUnknownKey    = errors.New("a key that is not known")
	errUserDuplicateKey  = errors.New("a key that is set twice")
	errUserWrongType     = errors.New("a value of the wrong type for its key")
	errUserOtherProblem  = errors.New("a key or value that cannot be used")
)

// maxNameLength is the longest accepted user name.
const maxNameLength = 64

// RecheckInterval is how long what the server knows of a user is used before the file of the
// user is checked again. A changed share list, a disabled user or a removed file takes effect
// this long after the change.
const RecheckInterval = 30 * time.Second

// User is one account read from the users directory.
type User struct {
	// ID is the id of the user, a UUIDv7, which is the file name without ".yaml". A token
	// names its user by the id, so that the name can stay a label.
	ID string
	// Name is the name the user logs in with.
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
	return api.SharesAllow(u.Shares, share)
}

// userFile is the YAML layout of one user file.
type userFile struct {
	Name         string   `yaml:"name"`
	PasswordHash string   `yaml:"password_hash"`
	Shares       []string `yaml:"shares"`
	Disabled     bool     `yaml:"disabled"`
}

// Users reads the user files of a directory, one file for each user, named by the user id.
// It is safe for concurrent use.
type Users struct {
	log *slog.Logger
	dir string
	now func() time.Time
	// stat is os.Stat; a test replaces it to count the checks of a file and to fail them.
	stat func(path string) (fs.FileInfo, error)

	mu     sync.Mutex
	files  *datadir.Dir[User]
	byName map[string]User
	byID   map[string]known
	merged bool
}

// known is what is remembered of one user id: the user, if there is one, the stamp of the
// file the user was read from, and when the file was last checked.
type known struct {
	user    User
	found   bool
	stamp   datadir.Stamp
	checked time.Time
}

// NewUsers returns a Users that reads "<dir>/<user id>.yaml". now tells the time, which
// decides when the file of a user is checked again. Problems with a file are logged to log.
func NewUsers(dir string, now func() time.Time, log *slog.Logger) *Users {
	return &Users{
		log: log, dir: dir, now: now, stat: os.Stat,
		files: datadir.NewDir(dir, parseUser, log), byID: map[string]known{},
	}
}

// Lookup returns the user with the given name, and whether there is one. The directory is read
// again first, so added, changed and removed users take effect at once. A name that is not a
// valid user name has no user. A file that cannot be read or parsed is ignored with a warning,
// and its last good content stays in use. Of two files with the same name, the file that sorts
// first by name is used. The result does not share memory with the cache.
func (u *Users) Lookup(name string) (User, bool) {
	if !validName(name) {
		return User{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refresh(u.now())
	user, found := u.byName[name]
	user.Shares = slices.Clone(user.Shares)
	return user, found
}

// Current returns the user with the given id, and whether there is one, as the server last
// knew it. The file of a user is checked with one stat when RecheckInterval has passed since
// the last check, and read again only when its stamp changed, so a request costs no file
// system call in between. A missing or unreadable file is a user that is gone. A stat that
// fails for any other reason is returned as an error and nothing is remembered: the user is
// neither let in nor refused on a guess. The result does not share memory with the cache.
func (u *Users) Current(id string) (User, bool, error) {
	if !uuid7.Valid(id) {
		return User{}, false, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	entry, err := u.entry(id, u.now())
	if err != nil {
		return User{}, false, err
	}
	user := entry.user
	user.Shares = slices.Clone(user.Shares)
	return user, entry.found, nil
}

// entry returns what is known of the user id, checking the file when the last check is old
// enough or there is none.
func (u *Users) entry(id string, now time.Time) (known, error) {
	entry, seen := u.byID[id]
	if !seen {
		return u.reload(id, now), nil
	}
	if now.Sub(entry.checked) < RecheckInterval {
		return entry, nil
	}
	same, err := u.unchanged(id, entry)
	switch {
	case err != nil:
		return known{}, err
	case !same:
		return u.reload(id, now), nil
	}
	entry.checked = now
	u.byID[id] = entry
	return entry, nil
}

// unchanged reports whether the file of the user id is as it was when entry was made: still
// missing, or still of the same stamp.
func (u *Users) unchanged(id string, entry known) (bool, error) {
	info, err := u.stat(filepath.Join(u.dir, datadir.IDFile(id)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return !entry.found, nil
	case err != nil:
		return false, fmt.Errorf("check user file: %w", err)
	}
	return entry.found && datadir.StampOf(info) == entry.stamp, nil
}

// reload reads the folder again and returns what is then known of the user id, which is
// remembered as checked now even when there is no such user, so that a flood of requests with
// the token of a removed user costs one check in the interval.
func (u *Users) reload(id string, now time.Time) known {
	u.refresh(now)
	entry, found := u.byID[id]
	if !found {
		entry = known{checked: now}
		u.byID[id] = entry
	}
	return entry
}

// refresh brings the users up to date with the folder. Reading the folder looks at every file,
// so every user then counts as checked at now, with the stamp of the file as it was found.
func (u *Users) refresh(now time.Time) {
	entries, changed := u.files.Refresh()
	if changed || !u.merged {
		u.byName, u.merged = u.merge(entries), true
	}
	u.byID = u.index(entries, now)
}

// index returns the users of the files by id, as checked at now. A file whose name another
// file took first is not a user.
func (u *Users) index(entries []datadir.Entry[User], now time.Time) map[string]known {
	byID := make(map[string]known, len(entries))
	for _, entry := range entries {
		if u.byName[entry.Value.Name].ID != entry.Value.ID {
			continue
		}
		byID[entry.Value.ID] = known{user: entry.Value, found: true, stamp: entry.Stamp, checked: now}
	}
	return byID
}

// merge indexes the users of the files, which are in file name order, by name.
func (u *Users) merge(entries []datadir.Entry[User]) map[string]User {
	byName := make(map[string]User, len(entries))
	owner := make(map[string]string, len(entries))
	for _, entry := range entries {
		if first, taken := owner[entry.Value.Name]; taken {
			u.log.Warn("duplicate user name, keeping the first file's user",
				slog.String("name", entry.Value.Name), slog.String("kept", first),
				slog.String("ignored", entry.Name))
			continue
		}
		owner[entry.Value.Name] = entry.Name
		byName[entry.Value.Name] = entry.Value
	}
	return byName
}

// parseUser validates one user file. The name of the file must be a user id, so that a file
// made by an earlier version, which was named for the user, never makes a user.
func parseUser(file string, data []byte) (User, error) {
	id, ok := datadir.IDOfFile(file)
	if !ok {
		return User{}, fmt.Errorf("users/%s: %w", file, errNotUserFile)
	}
	var parsed userFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return User{}, describeUserFileError(err)
	}
	switch {
	case parsed.Name == "":
		return User{}, errEmptyName
	case !validName(parsed.Name):
		return User{}, errBadName
	case parsed.PasswordHash == "":
		return User{}, errEmptyPasswordHash
	}
	return User{
		ID: id, Name: parsed.Name, PasswordHash: parsed.PasswordHash,
		Shares: parsed.Shares, Disabled: parsed.Disabled,
	}, nil
}

// describeUserFileError says where in a user file the decoder found a problem and what kind
// it is, without any text of the file. A type error lists one message per problem and the
// first one is reported with the count of the others.
func describeUserFileError(err error) error {
	var typeErr *yaml.TypeError
	switch {
	case errors.Is(err, io.EOF):
		return errUserFileEmpty
	case errors.As(err, &typeErr):
		return describeTypeErrors(typeErr.Errors)
	default:
		return atLine(errUserNotYAML, lineNumber(strings.TrimPrefix(err.Error(), "yaml: ")))
	}
}

// describeTypeErrors reports the first of the messages of a yaml.TypeError and how many more
// there are.
func describeTypeErrors(messages []string) error {
	if len(messages) == 0 {
		return errUserOtherProblem
	}
	first := userProblem(messages[0])
	if more := len(messages) - 1; more > 0 {
		return fmt.Errorf("%w (and %d more)", first, more)
	}
	return first
}

// userProblem classifies one message of a yaml.TypeError by the words the decoder puts first,
// which are its own, and keeps nothing after them, where it quotes the file.
func userProblem(message string) error {
	line := lineNumber(message)
	text := message
	if line > 0 {
		_, text, _ = strings.Cut(message, ": ")
	}
	switch {
	case strings.HasPrefix(text, "cannot unmarshal "):
		return atLine(errUserWrongType, line)
	case strings.HasPrefix(text, "field ") && strings.Contains(text, " not found in type "):
		return atLine(errUserUnknownKey, line)
	case strings.HasPrefix(text, "mapping key "):
		return atLine(errUserDuplicateKey, line)
	default:
		return atLine(errUserOtherProblem, line)
	}
}

// lineNumber reads the "line N: " the decoder puts before a message. It is 0 when the message
// has no such start.
func lineNumber(message string) int {
	rest, found := strings.CutPrefix(message, "line ")
	if !found {
		return 0
	}
	digits, _, found := strings.Cut(rest, ": ")
	if !found {
		return 0
	}
	line, err := strconv.Atoi(digits)
	if err != nil || line < 1 {
		return 0
	}
	return line
}

// atLine puts the line number before the problem, when there is one.
func atLine(problem error, line int) error {
	if line == 0 {
		return problem
	}
	return fmt.Errorf("line %d: %w", line, problem)
}

// validName reports whether name matches ^[a-z0-9][a-z0-9._-]{0,63}$, the form of a user name.
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
