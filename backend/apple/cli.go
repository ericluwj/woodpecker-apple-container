package apple

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// runner deliberately returns no CLI stderr or argument list in errors: either
// can contain workflow secrets. Workload output is handled by the log spool.
type runner interface {
	run(context.Context, ...string) ([]byte, error)
	start(context.Context, io.Writer, ...string) (*process, error)
}

type process struct {
	done chan struct{}
	code int
	err  error
}

type execRunner struct{ binary string }

func (r *execRunner) command(ctx context.Context, args []string) *exec.Cmd {
	c := exec.CommandContext(ctx, r.binary, args...)
	// Do not inherit flags that enable debug output or change the requested platform.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "CONTAINER_DEBUG" && key != "CONTAINER_DEFAULT_PLATFORM" {
			c.Env = append(c.Env, value)
		}
	}
	return c
}

func (r *execRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	c := r.command(ctx, args)
	out, err := c.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("container %s failed", args[0])
	}
	return out, nil
}

func (r *execRunner) start(ctx context.Context, output io.Writer, args ...string) (*process, error) {
	c := r.command(ctx, args)
	c.Stdout, c.Stderr = output, output
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start container CLI failed")
	}
	p := &process{done: make(chan struct{})}
	go func() {
		err := c.Wait() // also waits for os/exec's output copier to finish
		if ctx.Err() != nil {
			p.err = ctx.Err()
		} else if exit, ok := err.(*exec.ExitError); ok {
			p.code = exit.ExitCode()
			if p.code < 0 {
				p.err = fmt.Errorf("container CLI terminated by signal")
			}
		} else {
			p.err = err
		}
		close(p.done)
	}()
	return p, nil
}
