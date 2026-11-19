package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/api"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/internal/uuid7"
)

// dataPaths are the directories of the data the server keeps, below the state directory.
// "local" is for this machine only; "shared" may be copied between servers by any sync tool,
// which is why every file in it has exactly one writer.
type dataPaths struct {
	nodeIDFile, digestsDir, usersDir, keysDir, nodesDir string
	// folders lists every folder the server makes, parents first, with its mode.
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
		nodeIDFile: filepath.Join(local, "node-id"),
		digestsDir: filepath.Join(local, "digests"),
		usersDir:   filepath.Join(shared, "users"),
		keysDir:    filepath.Join(shared, "keys"),
		nodesDir:   filepath.Join(shared, "nodes"),
	}
	paths.folders = []folder{
		{stateDir, 0o750},
		{local, 0o700},
		{paths.digestsDir, 0o700},
		{shared, 0o750},
		{paths.usersDir, 0o750},
		{paths.keysDir, 0o700},
		{paths.nodesDir, 0o755},
	}
	return paths
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
	data, err := os.ReadFile(d.nodeIDFile)
	if errors.Is(err, fs.ErrNotExist) {
		return d.createNodeID(now(), random)
	}
	if err != nil {
		return "", fmt.Errorf("read node id file: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if !uuid7.Valid(id) {
		return "", fmt.Errorf("node id file %s %w", d.nodeIDFile, ErrMalformedNodeID)
	}
	return id, nil
}

// createNodeID makes a new id and saves it.
func (d *dataPaths) createNodeID(now time.Time, random io.Reader) (string, error) {
	id, err := uuid7.New(now, random)
	if err != nil {
		return "", fmt.Errorf("make node id: %w", err)
	}
	if err := datadir.WriteAtomic(d.nodeIDFile, []byte(id+"\n"), nodeIDMode); err != nil {
		return "", fmt.Errorf("write node id file %s: %w", d.nodeIDFile, err)
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
