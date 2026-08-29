package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joshgriffith1124/strawboss/internal/supervisor"
)

// runChat is the M1 console harness: a multi-turn conversation with the
// headless supervisor, every stream event printed as it parses, with running
// token totals. It exists to prove the driver before any TUI work.
func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	claudeBin := fs.String("claude", "claude", "claude binary to spawn")
	permMode := fs.String("permission-mode", "", "--permission-mode for the supervisor")
	allowed := fs.String("allowed-tools", "", "comma-separated --allowedTools")
	resume := fs.String("resume", "", "session id to resume")
	dir := fs.String("dir", "", "supervisor working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := &supervisor.Driver{
		Command:        *claudeBin,
		PermissionMode: *permMode,
		Dir:            *dir,
	}
	if *allowed != "" {
		d.AllowedTools = strings.Split(*allowed, ",")
	}
	if *resume != "" {
		d.SetSessionID(*resume)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	defer signal.Stop(sigs)

	fmt.Println("strawboss chat — ctrl-d or 'quit' to exit; ctrl-c interrupts a turn")
	var total supervisor.Usage
	var cost float64
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !in.Scan() {
			fmt.Println()
			return in.Err()
		}
		prompt := strings.TrimSpace(in.Text())
		if prompt == "" {
			continue
		}
		if prompt == "quit" || prompt == "exit" {
			return nil
		}

		turn, err := d.Start(prompt)
		if err != nil {
			return err
		}
		turnUsage, turnCost, err := renderTurn(turn, sigs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "turn failed:", err)
			continue
		}
		total = total.Add(turnUsage)
		cost += turnCost
		fmt.Printf("[session %s · running total %s tok · notional $%.4f]\n",
			short(d.SessionID()), formatTokens(total.Total()), cost)
	}
}

// renderTurn drains one turn's events to the console. SIGINT on sigs
// interrupts the turn (the session stays resumable).
func renderTurn(turn *supervisor.Turn, sigs <-chan os.Signal) (supervisor.Usage, float64, error) {
	var usage supervisor.Usage
	var cost float64
	streaming := false
	endStream := func() {
		if streaming {
			fmt.Println()
			streaming = false
		}
	}
	for {
		select {
		case <-sigs:
			fmt.Println("\n[interrupt]")
			turn.Interrupt()
		case ev, ok := <-turn.Events:
			if !ok {
				return usage, cost, nil
			}
			switch e := ev.(type) {
			case supervisor.InitEvent:
				fmt.Printf("[%s · %s · auth:%s · session %s]\n",
					e.ClaudeCodeVersion, e.Model, authLabel(e.APIKeySource), short(e.SessionID))
			case supervisor.StatusEvent:
				// quiet — chatter, not signal
			case supervisor.StreamDeltaEvent:
				if !streaming {
					fmt.Print("sup> ")
					streaming = true
				}
				fmt.Print(e.Text)
			case supervisor.AssistantEvent:
				endStream()
				for _, tu := range e.ToolUses {
					if cmd, ok := tu.BashCommand(); ok {
						fmt.Printf("  → bash %s\n", cmd)
					} else {
						fmt.Printf("  → %s %s\n", tu.Name, truncate(string(tu.Input), 100))
					}
				}
			case supervisor.ToolResultsEvent:
				for _, r := range e.Results {
					mark := "✔"
					if r.IsError {
						mark = "✖"
					}
					fmt.Printf("  ← %s %s\n", mark, truncate(r.Content, 120))
				}
			case supervisor.RateLimitEvent:
				fmt.Printf("  [plan window: 5h %.0f%% · 7d %.0f%%]\n",
					e.FiveHour.Utilization*100, e.SevenDay.Utilization*100)
			case supervisor.ResultEvent:
				endStream()
				usage = usage.Add(e.Usage)
				cost += e.TotalCostUSD
				fmt.Printf("[turn: in %s (cache-read %s) · out %s · notional $%.4f · %s]\n",
					formatTokens(e.Usage.InputTokens+e.Usage.CacheCreationTokens+e.Usage.CacheReadTokens),
					formatTokens(e.Usage.CacheReadTokens),
					formatTokens(e.Usage.OutputTokens), e.TotalCostUSD, e.Subtype)
			case supervisor.UnknownEvent:
				fmt.Printf("  [unparsed %s event]\n", e.Type)
			case supervisor.TurnDoneEvent:
				endStream()
				if e.ExitErr != nil {
					return usage, cost, fmt.Errorf("%w; stderr: %s", e.ExitErr, truncate(e.Stderr, 400))
				}
				if e.Interrupted {
					fmt.Println("[turn interrupted — session resumable]")
				}
			}
		}
	}
}

func authLabel(src string) string {
	if src == "none" {
		return "subscription"
	}
	return "API KEY (" + src + ") — invariant violated!"
}

func short(sid string) string {
	if len(sid) > 8 {
		return sid[:8] + "…"
	}
	return sid
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
