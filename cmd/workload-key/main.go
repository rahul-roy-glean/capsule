// workload-key computes workload keys from snapshot commands JSON.
// Used by dev scripts to align snapshot-builder keys with layered-config keys.
//
// Usage:
//
//	workload-key '[{"type":"shell","args":["echo","hello"]}]'
//	workload-key --leaf '[{"type":"shell","args":["echo","hello"]}]'
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

func main() {
	leaf := false
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--leaf" {
		leaf = true
		args = args[1:]
	}

	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: workload-key [--leaf] '<commands-json>'\n")
		os.Exit(1)
	}

	var commands []snapshot.SnapshotCommand
	if err := json.Unmarshal([]byte(args[0]), &commands); err != nil {
		fmt.Fprintf(os.Stderr, "invalid commands JSON: %v\n", err)
		os.Exit(1)
	}

	if leaf {
		layerHash := snapshot.ComputeLayerHash("", commands, nil)
		fmt.Println(snapshot.ComputeLeafWorkloadKey(layerHash))
	} else {
		fmt.Println(snapshot.ComputeWorkloadKey(commands))
	}
}
