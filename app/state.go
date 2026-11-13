package app

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/api"
	"github.com/SpliceFS/server/internal/datadir"
)

// dataPaths are the directories of the data the server keeps, below the state directory.
// "local" is for this machine only; "shared" may be copied between servers by any sync tool,
// which is why every file in it has exactly one writer.
type dataPaths struct {
	nodeIDFile, usersDir, keysDir, nodesDir string
	// folders lists every folder the server makes, parents first, with its mode.
	folders []folder
}

// folder is a folder the server makes if it is missing.
type folder struct {
	path string
	mode os.FileMode
}

// nodeIDBytes is the number of random bytes of a node id; the id is their hexadecimal form.
const nodeIDBytes = 8

// nodeIDMode is the mode of the node id file. Only this machine's server reads it.
const nodeIDMode = 0o600

// ErrNotAFolder is the cause of a start that fails because a file sits where the server
// needs a folder. The text goes after the path.
var ErrNotAFolder = errors.New("exists and is not a folder")

// ErrMalformedNodeID is the cause of a start that fails because the saved node id is not an
// id. The text goes after the name of the file.
var ErrMalformedNodeID = errors.New("must hold 16 lowercase hexadecimal characters")

// layout returns the data paths inside stateDir.
func layout(stateDir string) dataPaths {
	local := filepath.Join(stateDir, localDirName)
	shared := filepath.Join(stateDir, "shared")
	paths := dataPaths{
		nodeIDFile: filepath.Join(local, "node-id"),
		usersDir:   filepath.Join(shared, "users"),
		keysDir:    filepath.Join(shared, "keys"),
		nodesDir:   filepath.Join(shared, "nodes"),
	}
	paths.folders = []folder{
		{stateDir, 0o750},
		{local, 0o700},
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
func (d *dataPaths) nodeID(random io.Reader) (string, error) {
	data, err := os.ReadFile(d.nodeIDFile)
	if errors.Is(err, fs.ErrNotExist) {
		return d.createNodeID(random)
	}
	if err != nil {
		return "", fmt.Errorf("read node id file: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if !datadir.IsNodeID(id) {
		return "", fmt.Errorf("node id file %s %w", d.nodeIDFile, ErrMalformedNodeID)
	}
	return id, nil
}

// createNodeID makes a new id and saves it.
func (d *dataPaths) createNodeID(random io.Reader) (string, error) {
	raw := make([]byte, nodeIDBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("make node id: %w", err)
	}
	id := hex.EncodeToString(raw)
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
