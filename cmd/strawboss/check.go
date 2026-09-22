package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/joshgriffith1124/strawboss/internal/verify"
)

// runCheck runs the project's verify command once, outside of a worker
// lifecycle, and prints the same one bracketed note the supervisor reads
// from delegate. It exists so the supervisor can get the answer in a few
// dozen tokens instead of delegating an LLM worker to find out.
func runCheck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dir := fs.String("dir", "", "project directory (default: current directory)")
	verifyFlag := fs.String("verify", "", "check command, overriding the project's "+verify.FileName+" verify")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *verifyFlag == "off" {
		return errors.New("check: --verify off disables the check; nothing to run")
	}
	if *dir == "" {
		var err error
		if *dir, err = os.Getwd(); err != nil {
			return fmt.Errorf("check: %w", err)
		}
	}
	spec, err := verifySpec(*dir, *verifyFlag)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if spec.Command == "" {
		fmt.Fprintf(stdout, "no verify check defined — add verify = \"<command>\" to %s\n", verify.FileName)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res := verify.Run(ctx, *dir, spec)
	fmt.Fprintln(stdout, verifyNote(spec, res))
	if !res.OK {
		return errors.New("check failed")
	}
	return nil
}
