// strawboss runs a Claude Code supervisor that delegates coding tasks to
// local AI workers and visualizes the whole operation in a TUI.
//
// Subcommands:
//
//	strawboss           launch the TUI (M4; not yet implemented)
//	strawboss chat      console supervisor driver (M1 spike)
//	strawboss delegate  the command the supervisor calls to spawn a worker (M3)
//	strawboss version   print version
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	var err error
	switch cmd {
	case "":
		err = fmt.Errorf("TUI not implemented yet (M4) — try 'strawboss chat'")
	case "chat":
		err = runChat(args[1:])
	case "delegate":
		err = fmt.Errorf("delegate not implemented yet (M3)")
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
  version    print version
`)
}
