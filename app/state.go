package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// dataPaths are the directories of the data the server keeps, below the state directory.
// "local" is for this machine only; "shared" may be copied between servers by any sync tool,
// which is why every file in it has exactly one writer.
type dataPaths struct {
	nodeIDFile, nodeKeysFile, digestsDir, usersDir, nodesDir, clusterDir string
	// oldKeysDir held the token keys of earlier versions. It is not one of the folders: nothing
	// makes it any more, and removeOldKeys cleans it up.
	oldKeysDir string
	// folders lists every folder the server makes, parents first, with its mode. The digest
	// folder is not one of them: the digest store makes it.
	folders []folder
}

// folder is a folder the server makes if it is missing.
type folder struct {
	path string
	mode os.FileMode
}

// nodeIDMode is the mode of the node id file. Only this machine's server reads it.
const nodeIDMode = 0o600

// ErrNotAFolder is the cause of a start that fails because a file sits where the server
// needs a folder. The text goes after the path.
var ErrNotAFolder = errors.New("exists and is not a folder")

// ErrMalformedNodeID is the cause of a start that fails because the saved node id is not an
// id. The text goes after the name of the file.
var ErrMalformedNodeID = errors.New("must hold a UUIDv7 in lowercase")

// layout returns the data paths inside stateDir.
func layout(stateDir string) dataPaths {
	local := filepath.Join(stateDir, localDirName)
	shared := filepath.Join(stateDir, "shared")
	paths := dataPaths{
		nodeIDFile:   filepath.Join(local, "node-id"),
		nodeKeysFile: filepath.Join(local, "node-keys.yaml"),
		digestsDir:   filepath.Join(local, "digests"),
		usersDir:     filepath.Join(shared, "users"),
		nodesDir:     filepath.Join(shared, "nodes"),
		clusterDir:   filepath.Join(shared, "cluster"),
		oldKeysDir:   filepath.Join(shared, "keys"),
	}
	paths.folders = []folder{
		{stateDir, 0o750},
		{local, 0o700},
		{shared, 0o750},
		{paths.usersDir, 0o750},
		{paths.nodesDir, 0o755},
		{paths.clusterDir, 0o750},
	}
	return paths
}

// UsersDir returns the folder of the user files inside stateDir. The server reads it, and the
// tools that make user files write to it, so the path is stated once, in the layout.
func UsersDir(stateDir string) string {
	return layout(stateDir).usersDir
}

// ClusterDir returns the folder of the cluster key inside stateDir. The server reads it, and the
// tool that makes the cluster key writes to it, so the path is stated once, in the layout.
func ClusterDir(stateDir string) string {
	return layout(stateDir).clusterDir
}

// makeFolders creates the folders that are missing. A folder that exists keeps its mode,
// because the operator or the package may have set it on purpose.
func (d *dataPaths) makeFolders() error {
	for _, spec := range d.folders {
		if err := makeFolder(spec); err != nil {
			return err
		}
	}
	return nil
}

// makeFolder creates one folder with exactly its mode, not the mode that the umask leaves.
// A folder that exists is left alone, but a file in its place is an error here, because
// otherwise the failure would only show up later, far from its cause.
func makeFolder(spec folder) error {
	err := os.Mkdir(spec.path, spec.mode)
	if errors.Is(err, fs.ErrExist) {
		return checkFolder(spec.path)
	}
	if err != nil {
		return wrap(err, "create folder")
	}
	return wrap(os.Chmod(spec.path, spec.mode), "create folder")
}

// checkFolder fails if path is not a folder.
func checkFolder(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return wrap(err, "create folder")
	}
	if !info.IsDir() {
		return fmt.Errorf("create folder: %s %w", path, ErrNotAFolder)
	}
	return nil
}

// nodeID returns the id of this node, making and saving it on the first start. An id that
// is saved but malformed is an error and is never replaced: the id names this node's files
// in the shared folder, and a new one would orphan them.
func (d *dataPaths) nodeID(now func() time.Time, random io.Reader) (string, error) {
	return localValue[string]{
		what:  "node id",
		path:  d.nodeIDFile,
		mode:  nodeIDMode,
		parse: parseNodeID,
		create: func() ([]byte, error) {
			id, err := uuid7.New(now(), random)
			return []byte(id + "\n"), err
		},
	}.load()
}

// removeOldKeys removes the token keys file that earlier versions made for this node, and the
// folder of those files once it is empty. This server is the only writer of its own file; the
// file of another node is left to that node, which removes it when it runs this version.
func (d *dataPaths) removeOldKeys(nodeID string) error {
	// Only a folder there is the one earlier versions made. Anything else at that path is not
	// this server's, and this server no longer uses the path, so it is left alone.
	if !isFolder(d.oldKeysDir) {
		return nil
	}
	err := wrap(removeFile(filepath.Join(d.oldKeysDir, datadir.IDFile(nodeID))), "remove old token keys")
	if err != nil {
		return err
	}
	return wrap(removeEmptyFolder(d.oldKeysDir), "remove old token keys folder")
}

// isFolder reports whether path is a folder itself, not a link to one.
func isFolder(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// removeFile removes the file at path. A file that is not there is what was wanted.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// removeEmptyFolder removes the folder at path if it is empty. A folder that is not there is
// what was wanted, and a folder that holds files stays: the files are not this server's.
func removeEmptyFolder(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("remove folder: %w", err)
	}
	return nil
}

// parseNodeID returns the id that the content of a node id file holds.
func parseNodeID(data []byte) (string, error) {
	id := strings.TrimSpace(string(data))
	if !uuid7.Valid(id) {
		return "", ErrMalformedNodeID
	}
	return id, nil
}

// namedCluster serves the node list under the cluster name of the configuration, so that
// the name is set in one place and not in a file that every node must repeat.
type namedCluster struct {
	name  string
	nodes api.NodeLister
}

// Nodes returns the node list with the configured cluster name.
func (c namedCluster) Nodes() (wire.Nodes, error) {
	nodes, err := c.nodes.Nodes()
	nodes.Cluster = c.name
	if err != nil {
		return nodes, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}
