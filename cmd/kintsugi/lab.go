package main

// `kintsugi lab up|down|status`: the disposable lab harness (DESIGN §6.3,
// M8 remainder).
//
// The lab is where the work is executed: a compose-managed fixture
// environment the agent can run tooling against, never on the host. `up`
// brings it up and writes the lock that makes the boundary real, `down`
// tears it down (containers and volumes) and removes the lock, and `status`
// reports what the harness knows.
//
// The runtime, the compose file and the project name come from the [lab]
// block of the operator's configuration. Nothing here takes them from a
// model.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"kintsugi/internal/lab"
)

func cmdLab(ws string, args []string) {
	if len(args) == 0 {
		printLabUsage()
		return
	}
	switch args[0] {
	case "up":
		labUp(ws)
	case "down":
		labDown(ws)
	case "status":
		labStatus(ws)
	case "help", "-h", "--help":
		printLabUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown lab subcommand %q\n\n", args[0])
		printLabUsage()
		os.Exit(2)
	}
}

// labUp brings the lab up and prints what the operator needs to know: the
// runtime, the project, the services and the lock file.
func labUp(ws string) {
	cfg := labConfig(ws)
	state, err := lab.Up(context.Background(), ws, cfg)
	if err != nil {
		fail("lab up: %v", err)
	}
	fmt.Printf("lab up · %s\n", state.Runtime)
	fmt.Printf("project  %s\n", state.Project)
	fmt.Printf("services %s\n", labServices(state.Services))
	fmt.Printf("lock     %s\n", lab.LockPath(ws))
	fmt.Println("The lab lock is up: host commands are refused until `kintsugi lab down`.")
}

// labDown tears the lab down and reports what it removed.
func labDown(ws string) {
	cfg := labConfig(ws)
	rep, err := lab.DownReport(context.Background(), ws, cfg)
	if err != nil {
		fail("lab down: %v", err)
	}
	if !rep.LockFound {
		// Nothing was locked, so nothing was removed. The compose project is
		// still checked: a lab that crashed without leaving a lock must not
		// survive the teardown.
		fmt.Println("lab down · nothing was up")
		fmt.Printf("lock     %s (absent)\n", rep.LockPath)
		if rep.Teardown {
			fmt.Printf("checked  project %s (compose down -v)\n", rep.Project)
		}
		return
	}
	fmt.Printf("lab down · %s\n", rep.Runtime)
	fmt.Printf("removed  project %s%s\n", rep.Project, labServiceSuffix(rep.Services))
	if rep.Teardown {
		fmt.Printf("removed  containers and volumes (compose down -v)\n")
	} else {
		fmt.Printf("skipped  containers and volumes (%s)\n", rep.SkipReason)
	}
	if rep.LockRemoved {
		fmt.Printf("removed  lock %s\n", rep.LockPath)
	} else {
		fmt.Printf("kept     lock %s\n", rep.LockPath)
	}
}

// labStatus reports whether a lab is up, on what runtime, and how long the
// lock has been there.
func labStatus(ws string) {
	cfg := labConfig(ws)
	state, err := lab.Status(context.Background(), ws, cfg)
	if err != nil {
		fail("lab status: %v", err)
	}
	if !state.Up {
		fmt.Println("lab status · down")
		fmt.Printf("lock     %s (absent)\n", lab.LockPath(ws))
		return
	}
	fmt.Println("lab status · up")
	fmt.Printf("runtime  %s\n", state.Runtime)
	fmt.Printf("project  %s\n", state.Project)
	fmt.Printf("services %s\n", labServices(state.Services))
	fmt.Printf("started  %s (%s ago)\n",
		state.StartedAt.UTC().Format(time.RFC3339), labAge(time.Since(state.StartedAt)))
	fmt.Printf("lock     %s\n", lab.LockPath(ws))
}

// labConfig reads the [lab] block from the workspace configuration.
func labConfig(ws string) lab.Config {
	app := newApp(ws)
	return lab.FromConfig(app.Cfg.Lab)
}

// labServices renders the service list for the CLI.
func labServices(services []string) string {
	if len(services) == 0 {
		return "(every service in the compose file)"
	}
	return strings.Join(services, ", ")
}

// labServiceSuffix renders the service list as a suffix of a project line.
func labServiceSuffix(services []string) string {
	if len(services) == 0 {
		return ""
	}
	return " (services: " + strings.Join(services, ", ") + ")"
}

// labAge renders a duration in one word: seconds, minutes, hours, days.
func labAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func printLabUsage() {
	fmt.Print(labUsageText())
}

// labUsageText is the usage text, as a string so it can be checked.
func labUsageText() string {
	return `usage: kintsugi lab up|down|status

  kintsugi lab up         bring the lab up (compose) and write the lab lock
  kintsugi lab down       tear the lab down (containers and volumes) and remove the lock
  kintsugi lab status     report whether a lab is up, on what runtime, and for how long

The lab is configured in the [lab] block of kintsugi.toml:
  runtime      auto | docker | podman      (default: auto)
  compose_file lab/docker-compose.yml      (relative to the workspace)
  project      kintsugi-lab
  services     []                          (empty: every service in the file)
  network      ""                          (the network the compose file declares)

While the lock exists, host commands are refused: the agent runs commands in
the lab with the run_in_lab tool.
`
}
