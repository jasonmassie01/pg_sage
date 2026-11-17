// Package restorecmd turns a test binary into a fake operator restore
// program for clone.CommandProvider tests (drill-01c). A test points
// restore_argv or destroy_argv at Argv(mode, args...); the package's
// TestMain calls Main first, which acts as the program and exits when the
// binary was started that way.
package restorecmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Marker is the argument that makes a test binary act as the program.
const Marker = "sage-restore-helper"

// Variables a test passes through env_allow.
const (
	EnvDSN     = "RESTORE_HELPER_DSN"
	EnvMarker  = "RESTORE_HELPER_CANARY"
	envDrillID = "SAGE_DRILL_ID"
	envCloneID = "SAGE_CLONE_ID"
)

// Argv is the program line of this test binary acting in mode.
func Argv(mode string, args ...string) []string {
	return append([]string{os.Args[0], Marker, mode}, args...)
}

// Main acts as the program when the binary was started by Argv; it
// returns false otherwise, and the caller runs its tests.
func Main() bool {
	if len(os.Args) < 3 || os.Args[1] != Marker {
		return false
	}
	os.Exit(run(os.Args[2], os.Args[3:]))
	return true
}

func run(mode string, args []string) int {
	switch mode {
	case "ok", "drill-id":
		return printResult(mode)
	case "pad":
		return printPadded(args)
	case "garbage":
		fmt.Println("restore finished OK")
		return 0
	case "no-dsn":
		return printJSON(map[string]string{"id": "clone-1",
			"recovery_point": recoveryPoint()})
	case "dsn-arg":
		return printJSON(map[string]string{"dsn": firstArg(args), "id": "clone-src",
			"recovery_point": recoveryPoint()})
	case "fail":
		fmt.Fprintln(os.Stderr, "pgbackrest: connecting with postgres://backup:hunter2@db1/app")
		fmt.Fprintln(os.Stderr, "ERROR: repo locked password=hunter2")
		return 3
	case "hold-output":
		return holdOutput()
	case "linger":
		time.Sleep(8 * time.Second)
		return 0
	case "sleep":
		time.Sleep(10 * time.Minute)
		return 0
	case "env":
		return printEnv()
	case "destroy-ok":
		return destroyOK()
	case "destroy-fail":
		fmt.Fprintln(os.Stderr, "pg_ctl: could not stop server")
		return 1
	}
	fmt.Fprintln(os.Stderr, "restore helper: unknown mode", mode)
	return 2
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func recoveryPoint() string {
	return time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format(time.RFC3339)
}

func printJSON(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println(string(b))
	return 0
}

// printResult prints a valid line for EnvDSN; in drill-id mode the clone
// id is the SAGE_DRILL_ID this process was given, after a pause so two
// drills overlap.
func printResult(mode string) int {
	id := "clone-" + os.Getenv(envDrillID)
	if mode == "drill-id" {
		time.Sleep(300 * time.Millisecond)
		id = os.Getenv(envDrillID)
	}
	return printJSON(map[string]string{"dsn": os.Getenv(EnvDSN), "id": id,
		"recovery_point": recoveryPoint()})
}

// printPadded prints a valid line padded with leading blank lines to
// exactly args[0] bytes of stdout.
func printPadded(args []string) int {
	total, err := strconv.Atoi(firstArg(args))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pad needs a byte count")
		return 2
	}
	b, _ := json.Marshal(map[string]string{"dsn": os.Getenv(EnvDSN), "id": "clone-pad",
		"recovery_point": recoveryPoint()})
	line := string(b) + "\n"
	if total < len(line) {
		fmt.Fprintln(os.Stderr, "pad count below the line length")
		return 2
	}
	fmt.Print(strings.Repeat("\n", total-len(line)) + line)
	return 0
}

// printEnv writes the working directory and the environment names the
// process was given to env-<SAGE_DRILL_ID>.txt in its working directory,
// then prints a valid line.
func printEnv() int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 5
	}
	lines := []string{wd}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && name != "" {
			lines = append(lines, name)
		}
	}
	file := filepath.Join(wd, "env-"+os.Getenv(envDrillID)+".txt")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 5
	}
	return printResult("ok")
}

// destroyOK records the SAGE_CLONE_ID it was given in its working
// directory.
func destroyOK() int {
	id := os.Getenv(envCloneID)
	if id == "" {
		fmt.Fprintln(os.Stderr, "no SAGE_CLONE_ID")
		return 4
	}
	name := "destroyed-" + strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(id)
	if err := os.WriteFile(filepath.Join(".", name), []byte(id), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 5
	}
	return 0
}

// holdOutput starts a child that keeps this program's stdout and stderr
// open after it exits (a server started without a log file), then prints
// a valid line and exits 0.
func holdOutput() int {
	child := exec.Command(os.Args[0], Marker, "linger")
	child.Stdout, child.Stderr, child.Dir = os.Stdout, os.Stderr, os.TempDir()
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 5
	}
	return printResult("ok")
}
