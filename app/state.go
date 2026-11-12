package app

import (
	"path/filepath"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/api"
)

// dataPaths are the files and directories of the data the server reads.
//
// This is the interim layout: it keeps today's file formats under the shared folder of the
// state directory. The next change replaces it with one file per node under shared/keys and
// shared/nodes.
type dataPaths struct {
	usersDir, keysFile, nodesFile string
}

// layout returns the data paths inside stateDir.
func layout(stateDir string) dataPaths {
	shared := filepath.Join(stateDir, "shared")
	return dataPaths{
		usersDir:  filepath.Join(shared, "users"),
		keysFile:  filepath.Join(shared, "keys.yaml"),
		nodesFile: filepath.Join(shared, "nodes.yaml"),
	}
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
	return nodes, err
}
