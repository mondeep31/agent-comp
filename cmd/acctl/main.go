// Command acctl is the command-line client for the agent-comp API.
//
// It reads the API address from AC_URL (default http://127.0.0.1:8080) and the
// key from AC_API_KEY.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mondeep31/agent-comp/internal/api"
	"github.com/mondeep31/agent-comp/internal/auth"
	"github.com/mondeep31/agent-comp/internal/client"
	"github.com/mondeep31/agent-comp/internal/sandbox"
	"github.com/mondeep31/agent-comp/internal/version"
)

const usage = `usage: acctl <command> [args]

Keys:
  keygen [-append FILE] NAME    create an API key (token on stdout)

Sandboxes:
  create [flags]                create a sandbox and print it
  ls                            list sandboxes
  get ID                        show a sandbox
  rm ID...                      delete sandboxes
  pause ID | resume ID
  exec ID -- ARGV...            run a command, streaming stdin/stdout/stderr
  sh ID 'COMMAND'               run a shell command

Files:
  cat ID PATH                   print a file
  put ID LOCAL REMOTE           upload a file ("-" reads stdin)
  ls-files ID PATH              list a directory

Snapshots:
  snapshot ID                   snapshot a sandbox
  snapshots                     list snapshots
  rm-snapshot ID

  version
`

// exitError carries a remote command's exit status out of main.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	var code exitError
	switch {
	case err == nil:
	case errors.As(err, &code):
		os.Exit(int(code))
	default:
		fmt.Fprintln(os.Stderr, "acctl:", err)
		os.Exit(1)
	}
}

func newClient() (*client.Client, error) {
	key := os.Getenv("AC_API_KEY")
	if key == "" {
		return nil, errors.New("AC_API_KEY is not set")
	}
	base := os.Getenv("AC_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return client.New(base, key, nil), nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func needArgs(args []string, n int, form string) error {
	if len(args) != n {
		return fmt.Errorf("usage: acctl %s", form)
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Println(version.Version)
		return nil
	case "keygen":
		return keygen(args)
	}

	c, err := newClient()
	if err != nil {
		return err
	}
	switch cmd {
	case "create":
		return create(ctx, c, args)
	case "ls":
		list, err := c.List(ctx)
		if err != nil {
			return err
		}
		for _, s := range list {
			fmt.Printf("%s\t%s\t%s\t%dc/%dm\texpires %s\n", s.ID, s.State, s.Image, s.VCPU, s.MemoryMiB, s.ExpiresAt.Local().Format(time.DateTime))
		}
		return nil
	case "get", "pause", "resume", "snapshot":
		if err := needArgs(args, 1, cmd+" ID"); err != nil {
			return err
		}
		var v any
		switch cmd {
		case "get":
			v, err = c.Get(ctx, args[0])
		case "pause":
			v, err = c.Pause(ctx, args[0])
		case "resume":
			v, err = c.Resume(ctx, args[0])
		default:
			v, err = c.Snapshot(ctx, args[0])
		}
		if err != nil {
			return err
		}
		return printJSON(v)
	case "rm":
		for _, id := range args {
			if err := c.Delete(ctx, id); err != nil {
				return err
			}
		}
		return nil
	case "exec":
		if len(args) < 3 || args[1] != "--" {
			return errors.New("usage: acctl exec ID -- PROGRAM [ARGS]")
		}
		return stream(ctx, c, args[0], &sandbox.ExecRequest{Argv: args[2:]})
	case "sh":
		if err := needArgs(args, 2, "sh ID 'COMMAND'"); err != nil {
			return err
		}
		return stream(ctx, c, args[0], &sandbox.ExecRequest{Command: args[1]})
	case "cat":
		if err := needArgs(args, 2, "cat ID PATH"); err != nil {
			return err
		}
		return c.ReadFile(ctx, args[0], args[1], os.Stdout)
	case "put":
		if err := needArgs(args, 3, "put ID LOCAL REMOTE"); err != nil {
			return err
		}
		return put(ctx, c, args[0], args[1], args[2])
	case "ls-files":
		if err := needArgs(args, 2, "ls-files ID PATH"); err != nil {
			return err
		}
		entries, err := c.ListDir(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Printf("%s\t%s\t%d\t%s\n", e.Mode, e.Type, e.Size, e.Name)
		}
		return nil
	case "snapshots":
		list, err := c.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		return printJSON(list)
	case "rm-snapshot":
		if err := needArgs(args, 1, "rm-snapshot ID"); err != nil {
			return err
		}
		return c.DeleteSnapshot(ctx, args[0])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	appendTo := fs.String("append", "", "append the digest line to this keys file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: acctl keygen [-append FILE] NAME")
	}
	token, line, err := auth.Generate(fs.Arg(0))
	if err != nil {
		return err
	}
	if *appendTo == "" {
		fmt.Fprintf(os.Stderr, "add this line to the api keys file, then send acd SIGHUP:\n%s\n", line)
	} else {
		f, err := os.OpenFile(*appendTo, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, line); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "added %q to %s; send acd SIGHUP to load it\n", fs.Arg(0), *appendTo)
	}
	fmt.Println(token)
	return nil
}

func create(ctx context.Context, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	var req api.CreateSandboxRequest
	fs.StringVar(&req.Image, "image", "", "image name (default: server default)")
	fs.IntVar(&req.VCPU, "vcpu", 0, "vCPUs")
	fs.IntVar(&req.MemoryMiB, "memory", 0, "memory in MiB")
	fs.IntVar(&req.DiskMiB, "disk", 0, "disk in MiB")
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 30m")
	idle := fs.Duration("idle", 0, "pause after this much inactivity")
	fs.StringVar(&req.FromSnapshot, "from-snapshot", "", "fork a snapshot")
	quiet := fs.Bool("q", false, "print only the id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req.TTLSeconds, req.IdleTimeoutSeconds = int(ttl.Seconds()), int(idle.Seconds())
	info, err := c.Create(ctx, &req)
	if err != nil {
		return err
	}
	if *quiet {
		fmt.Println(info.ID)
		return nil
	}
	return printJSON(info)
}

func stream(ctx context.Context, c *client.Client, id string, req *sandbox.ExecRequest) error {
	var stdin io.Reader
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		stdin = os.Stdin
	}
	exit, err := c.ExecStream(ctx, id, req, stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	switch {
	case exit.TimedOut:
		fmt.Fprintln(os.Stderr, "acctl: command timed out")
		return exitError(124)
	case exit.Signal != 0:
		return exitError(128 + exit.Signal)
	case exit.ExitCode != nil && *exit.ExitCode != 0:
		return exitError(*exit.ExitCode)
	}
	return nil
}

func put(ctx context.Context, c *client.Client, id, local, remote string) error {
	var r io.Reader = os.Stdin
	var mode uint32
	if local != "-" {
		f, err := os.Open(local) //nolint:gosec // the user names their own local file
		if err != nil {
			return err
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			mode = uint32(fi.Mode().Perm())
		}
		r = f
	}
	fe, err := c.WriteFile(ctx, id, remote, mode, r)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes to %s\n", fe.Size, strings.TrimSpace(fe.Path))
	return nil
}
