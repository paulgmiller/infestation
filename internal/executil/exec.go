// Package executil isolates the remaining external CLIs behind a testable runner.
package executil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type Result struct {
	Stdout, Stderr string
	Code           int
}

func (r Result) Err(operation string) error {
	if r.Code == 0 {
		return nil
	}
	s := r.Stderr
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return fmt.Errorf("%s failed (%d): %s", operation, r.Code, s)
}

type Runner interface {
	Run(context.Context, string, ...string) Result
}
type OSRunner struct {
	Dir    string
	Env    []string
	Output io.Writer
}

func (r OSRunner) Run(ctx context.Context, name string, args ...string) Result {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	if r.Env != nil {
		cmd.Env = r.Env
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cmd.Stdin = os.Stdin
	if r.Output != nil {
		cmd.Stdout = io.MultiWriter(&out, r.Output)
		cmd.Stderr = io.MultiWriter(&stderr, r.Output)
	}
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if ex, ok := err.(*exec.ExitError); ok {
			code = ex.ExitCode()
		}
		if stderr.Len() == 0 {
			stderr.WriteString(err.Error())
		}
	}
	return Result{Stdout: out.String(), Stderr: stderr.String(), Code: code}
}
