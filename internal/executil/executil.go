// Package executil runs the small set of system commands breadpair needs and
// turns their failures into errors that name the command.
package executil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result is one finished command.
type Result struct {
	Args   []string
	Stdout string
	Stderr string
	Code   int
}

func (r Result) String() string {
	return fmt.Sprintf("%s (exit %d)\n%s%s", strings.Join(r.Args, " "), r.Code, r.Stdout, r.Stderr)
}

// Run executes args[0] with the rest as arguments. A non-zero exit is an error
// that includes both streams, trimmed, so the caller can show it directly.
func Run(ctx context.Context, args ...string) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := Result{
		Args:   args,
		Stdout: out.String(),
		Stderr: errb.String(),
	}
	if cmd.ProcessState != nil {
		res.Code = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return res, fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, tail(res.Stdout+res.Stderr))
	}
	return res, nil
}

// Sudo runs the command through sudo -n, so a missing sudoers entry fails
// immediately instead of hanging on a password prompt.
func Sudo(ctx context.Context, args ...string) (Result, error) {
	return Run(ctx, append([]string{"sudo", "-n"}, args...)...)
}

// StartSudo starts a long-running command and returns its process. The caller
// owns the process and must kill it.
func StartSudo(ctx context.Context, args ...string) (*os.Process, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n"}, args...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", strings.Join(args, " "), err)
	}
	return cmd.Process, nil
}

// Output is Run plus the trimmed stdout.
func Output(ctx context.Context, args ...string) (string, error) {
	res, err := Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Look reports whether name is on PATH.
func Look(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is not installed", name)
	}
	return nil
}

// Timeout is a convenience for the short commands that have no caller context.
func Timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 1200
	if len(s) <= max {
		return s
	}
	return "...\n" + s[len(s)-max:]
}
