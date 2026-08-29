// strawboss runs a Claude Code supervisor that delegates coding tasks to
// local AI workers and visualizes the whole operation in a TUI.
//
// Subcommands:
//
//	strawboss           launch the TUI
//	strawboss chat      console supervisor driver
//	strawboss delegate  the command the supervisor calls to spawn a worker
//	strawboss version   print version
package main

import (
	"fmt"
	"os"
	"strings"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "":
		err = runTUI(args)
	case "chat":
		err = runChat(args)
	case "delegate":
		err = runDelegate(args, os.Stdout)
	case "clean":
		err = runClean(args, os.Stdout)
	case "costs":
		err = runCosts(args, os.Stdout)
	case "version":
		fmt.Println("strawboss", version)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "strawboss:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: strawboss [command]

  (none)     launch the TUI
  chat       console supervisor driver (spike)
  delegate   spawn a worker (called by the supervisor)
  clean      retention sweep: old logs/sessions, merged worktrees
  costs      per-run worker token/time summary from the JSONL history
  version    print version
`)
}
