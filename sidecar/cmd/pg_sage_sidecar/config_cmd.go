package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// `pg_sage_sidecar config validate | diff | export` (cli-01b): the config
// file's checks without starting the sidecar, and the running sidecar's
// effective settings made visible as file YAML. validate is offline (no
// database, no sidecar). diff and export read a running sidecar's API
// with an operator API token whose owner is an admin (SAGE_API_TOKEN, or
// SAGE_API_TOKEN_FILE; SAGE_API_URL or --url), the same way as the
// decision-loop commands. All three only read: nothing is written to the
// file, the sidecar or a database.

const configSubcommand = "config"

// Exit codes: 2 is "invalid" for validate and "differs" for diff, so a CI
// gate can fail on either; a usage error is 64 so it never reads as 2.
const (
	configExitOK      = 0
	configExitError   = 1
	configExitProblem = 2
	configExitAuth    = 3
	configExitUsage   = 64
)

const (
	configValidateUsage = "usage: pg_sage_sidecar config validate <file|directory>"
	configDiffUsage     = "usage: pg_sage_sidecar config diff <file|directory> [--url URL]"
	configExportUsage   = "usage: pg_sage_sidecar config export [--url URL] [--only-overrides]"
	configUsage         = configValidateUsage + "\n" + configDiffUsage + "\n" +
		configExportUsage
)

// runConfigCommand runs `config <verb>`; args start after "config".
func runConfigCommand(ctx context.Context, args []string, env cliEnv) int {
	switch firstArg(args) {
	case "validate":
		return runConfigValidate(args[1:], env)
	case "diff":
		return runConfigDiff(ctx, args[1:], env)
	case "export":
		return runConfigExport(ctx, args[1:], env)
	case "help", "-h", "--help", "-help":
		_, _ = fmt.Fprintln(env.stdout, configUsage)
		return configExitOK
	}
	_, _ = fmt.Fprintf(env.stderr, "config: unknown verb %q\n%s\n", firstArg(args), configUsage)
	return configExitUsage
}

// parseFileArgs parses flags around one positional file argument (flags
// may come before or after it). done reports a run that ends with code.
func parseFileArgs(fs *flag.FlagSet, args []string, usage string,
	env cliEnv) (file string, code int, done bool) {
	if code, done := parseCLIFlags(fs, args, usage, configExitUsage, env); done {
		return "", code, true
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", usageError(env, fs.Name(), usage, configExitUsage, "name the config file"),
			true
	}
	file = rest[0]
	if code, done := parseCLIFlags(fs, rest[1:], usage, configExitUsage, env); done {
		return "", code, true
	}
	if fs.NArg() > 0 {
		return "", usageError(env, fs.Name(), usage, configExitUsage,
			"unexpected argument %q", fs.Arg(0)), true
	}
	return file, 0, false
}

// runConfigValidate loads and validates one file offline: exit 0 valid,
// 2 invalid (every problem printed with its key path), 1 unreadable.
func runConfigValidate(args []string, env cliEnv) int {
	fs := newFlagSet("config validate")
	file, code, done := parseFileArgs(fs, args, configValidateUsage, env)
	if done {
		return code
	}
	if isDirectory(file) {
		return validateDirectory(file, env) // a policy.directory (cli-02a)
	}
	check, err := config.CheckFile(file)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "config validate: %v\n", err)
		return configExitError
	}
	if len(check.Problems) > 0 {
		printProblems(env, file, check.Problems)
		return configExitProblem
	}
	if check.Empty {
		_, _ = fmt.Fprintf(env.stdout, "%s: valid; the file sets nothing, so every "+
			"setting is its default\n", file)
		return configExitOK
	}
	_, _ = fmt.Fprintf(env.stdout, "%s: valid\n", file)
	return configExitOK
}

func printProblems(env cliEnv, file string, problems []config.FileProblem) {
	noun := "problems"
	if len(problems) == 1 {
		noun = "problem"
	}
	_, _ = fmt.Fprintf(env.stdout, "%s: invalid, %d %s:\n", file, len(problems), noun)
	for _, p := range problems {
		_, _ = fmt.Fprintf(env.stdout, "  %s\n", p)
	}
}

// runConfigExport prints the running sidecar's effective settings, or
// only its stored overrides, as file YAML. Secrets are placeholders.
func runConfigExport(ctx context.Context, args []string, env cliEnv) int {
	fs := newFlagSet("config export")
	var f cliFlags
	var onlyOverrides bool
	fs.StringVar(&f.url, "url", "", "the sidecar's API (default $SAGE_API_URL)")
	fs.BoolVar(&onlyOverrides, "only-overrides", false, "only the stored overrides")
	if code, done := parseCLIFlags(fs, args, configExportUsage, configExitUsage,
		env); done {
		return code
	}
	if fs.NArg() > 0 {
		return usageError(env, fs.Name(), configExportUsage, configExitUsage,
			"unexpected argument %q", fs.Arg(0))
	}
	s, code := connect(fs.Name(), f, env, configExitAuth)
	if s == nil {
		return code
	}
	query := url.Values{}
	if onlyOverrides {
		query.Set("overrides", "1")
	}
	ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/config/yaml", query, nil)
	if err != nil {
		return s.fail(err, configExitAuth)
	}
	text, ok := ans.body["yaml"].(string)
	if !ok {
		_, _ = fmt.Fprintf(env.stderr, "config export: the sidecar's answer has no yaml; "+
			"is it older than this CLI?\n")
		return configExitError
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, _ = fmt.Fprint(env.stdout, s.api.redact(text))
	return configExitOK
}
