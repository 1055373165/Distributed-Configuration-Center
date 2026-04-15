// PaladinCore - Final entry point supporting both standalone and Raft cluster modes.
//
// Usage:
//
//	paladin-core serve [--addr :8080]                          Standalone mode (Day 1-3)
//	paladin-core cluster --id node1 --raft 127.0.0.1:9001      Raft cluster mode (Day 4-7)
//	              --http :8080 [--join leader:8080] [--bootstrap]
//	paladin-core put/get/delete/list/rev                        Local CLI (Day 1)
package main

import (
	"fmt"
	"just_for_test_only_once/paladin-core/store"
	"os"
)

const defaultDBPath = "paladin-core.db"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]

	switch cmd {
	case "put", "get", "delete", "list", "rev":
		runCLI(cmd)
	default:
		usage()
		os.Exit(1)
	}
}

func runCLI(cmd string) {
	s, err := store.NewBoltStore(defaultDBPath)
	if err != nil {
		fatal("open store: %v", err)
	}
	defer s.Close()

	switch cmd {
	case "put":
		if len(os.Args) < 4 {
			fatal("usage: paladin-core put <key> <value>")
		}
		res, err := s.Put(os.Args[2], []byte(os.Args[3]))
		if err != nil {
			fatal("put: %v", err)
		}
		fmt.Printf("OK rev=%d version=%d key=%s\n", res.Entry.Revision, res.Entry.Version, os.Args[2])
	case "get":
		if len(os.Args) < 3 {
			fatal("usage: paladin-core get <key>")
		}
		e, err := s.Get(os.Args[2])
		if err != nil {
			fatal("get: %v", err)
		}
		fmt.Printf("key=%s value=%s rev=%d\n", e.Key, string(e.Value), e.Revision)
	case "delete":
		if len(os.Args) < 3 {
			fatal("usage: paladin-core delete <key>")
		}
		d, err := s.Delete(os.Args[2])
		if err != nil {
			fatal("delete: %v", err)
		}
		fmt.Printf("DELETED key=%s rev=%d\n", os.Args[2], d.Revision)
	case "list":
		prefix := ""
		if len(os.Args) >= 3 {
			prefix = os.Args[2]
		}
		entries, err := s.List(prefix)
		if err != nil {
			fatal("list: %v", err)
		}
		for _, e := range entries {
			fmt.Printf("  %-30s = %-20s  rev=%d\n", e.Key, e.Value, e.Revision)
		}
	case "rev":
		fmt.Printf("rev=%d\n", s.Rev())
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `PaladinCore — Distributed Configuration Center

Commands:
  serve [addr]                  Standalone HTTP server
  cluster --id ID [options]     Raft cluster node

Cluster Options:
  --id ID              Node ID (required)
  --raft ADDR          Raft bind address (default 127.0.0.1:9001)
  --http ADDR          HTTP listen address (default :8080)
  --data DIR           Data directory (default data-{id})
  --bootstrap          Bootstrap as initial leader
  --join LEADER:PORT   Join an existing cluster

CLI Commands:
  put <key> <value>    Create/update config
  get <key>            Get config
  delete <key>         Delete config
  list [prefix]        List configs
  rev                  Show revision`)
}

func fatal(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...)
	os.Exit(1)
}
