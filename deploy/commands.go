package deploy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/everdict/herd/conformance/substrateapi"
)

// Command is one command line a herd binary accepts: Binary, and Sub when it is a subcommand.
type Command struct {
	Binary string
	Sub    string
	// Positional names the arguments it takes before or after its flags (herdlet join's herd-api address).
	Positional []string
	// Synopsis is a one-line description.
	Synopsis string
	// New registers the command's flags on fs and returns what runs it once they are parsed.
	New func(fs *flag.FlagSet) func(ctx context.Context, pos []string, out io.Writer) error
}

// Name is how the command is typed: "herdlet join".
func (c Command) Name() string {
	if c.Sub == "" {
		return c.Binary
	}
	return c.Binary + " " + c.Sub
}

func listenBoth(tcp, udp string) (net.Listener, net.PacketConn, error) {
	ln, err := net.Listen("tcp", tcp)
	if err != nil {
		return nil, nil, err
	}
	pc, err := net.ListenPacket("udp", udp)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	return ln, pc, nil
}

// Commands lists every command line of herd's binaries: herd-api, herd-router, herd-egress, herdlet and herd.
func Commands() []Command {
	return []Command{
		{Binary: "herd-api", Synopsis: "serve herd-api", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			a := &API{}
			a.Flags(fs)
			return func(ctx context.Context, _ []string, _ io.Writer) error {
				ln, err := net.Listen("tcp", a.Listen)
				if err != nil {
					return err
				}
				return a.Run(ctx, ln)
			}
		}},
		{Binary: "herd-api", Sub: "token", Synopsis: "mint a bearer token offline from the key", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			t := &Token{}
			t.Flags(fs)
			return func(_ context.Context, _ []string, out io.Writer) error {
				tok, err := t.Mint()
				if err == nil {
					fmt.Fprintln(out, tok)
				}
				return err
			}
		}},
		{Binary: "herd-api", Sub: "join-token", Synopsis: "mint the token hosts join with", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			j := &JoinToken{}
			j.Flags(fs)
			return func(_ context.Context, _ []string, out io.Writer) error {
				tok, err := j.Mint()
				if err == nil {
					fmt.Fprintln(out, tok)
				}
				return err
			}
		}},
		{Binary: "herd-api", Sub: "keygen", Synopsis: "write a random token signing key", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			k := &Keygen{}
			k.Flags(fs)
			return func(_ context.Context, _ []string, out io.Writer) error {
				_, err := EnsureKey(k.Out)
				if err == nil {
					fmt.Fprintln(out, k.Out)
				}
				return err
			}
		}},
		{Binary: "herd-api", Sub: "workers", Synopsis: "list the workers herd-api knows and what each declared", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			w := &Workers{}
			w.Flags(fs)
			return func(ctx context.Context, _ []string, out io.Writer) error {
				lines, err := w.List(ctx)
				for _, l := range lines {
					fmt.Fprintln(out, l)
				}
				return err
			}
		}},
		{Binary: "herd-router", Synopsis: "serve herd-router", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			r := &Router{}
			r.Flags(fs)
			return func(ctx context.Context, _ []string, _ io.Writer) error {
				tl, err := net.Listen("tcp", r.TunnelListen)
				if err != nil {
					return err
				}
				cl, err := net.Listen("tcp", r.Listen)
				if err != nil {
					tl.Close()
					return err
				}
				return r.Run(ctx, cl, tl)
			}
		}},
		{Binary: "herd-egress", Synopsis: "serve herd-egress", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			e := &Egress{}
			e.Flags(fs)
			return func(ctx context.Context, _ []string, _ io.Writer) error {
				ln, pc, err := listenBoth(e.Listen, e.DNSListen)
				if err != nil {
					return err
				}
				defer pc.Close()
				return e.Run(ctx, ln, pc)
			}
		}},
		{Binary: "herd-egress", Sub: "nft", Synopsis: "render one VM's TAP and nftables table", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			n := &NFT{}
			n.Flags(fs)
			return func(_ context.Context, _ []string, out io.Writer) error {
				rs, err := n.Render()
				if err == nil {
					fmt.Fprint(out, rs)
				}
				return err
			}
		}},
		{Binary: "herdlet", Sub: "join", Positional: []string{"herd-api"}, Synopsis: "join herd-api with a join token, then serve", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			h := &Herdlet{}
			h.JoinFlags(fs)
			return func(ctx context.Context, pos []string, _ io.Writer) error {
				if len(pos) != 1 {
					return errors.New("usage: herdlet join <herd-api host:port> --token <join token> [flags]")
				}
				h.API = pos[0]
				return h.Run(ctx)
			}
		}},
		{Binary: "herdlet", Sub: "run", Synopsis: "serve with -token-file, or with the grant an earlier join left in -state", New: herdletRun},
		{Binary: "herdlet", Synopsis: "herdlet run", New: herdletRun},
		{Binary: "herdlet", Sub: "doctor", Synopsis: "say whether this host can run herd actors and what it declares, without dialing", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			h := &Herdlet{}
			h.RunFlags(fs)
			return func(ctx context.Context, _ []string, out io.Writer) error {
				failed := 0
				for _, c := range Doctor(h, ThisHost()) {
					mark := "ok  "
					if !c.OK {
						mark, failed = "FAIL", failed+1
					}
					fmt.Fprintf(out, "%s %-26s %s\n", mark, c.Name, c.Detail)
				}
				// Then what the host declares, as the backend and the image builder see it.
				if err := h.Describe(ctx, out); err != nil {
					return fmt.Errorf("%d checks failed; backend: %w", failed, err)
				}
				if failed > 0 {
					return fmt.Errorf("%d checks failed: this host cannot join", failed)
				}
				return nil
			}
		}},
		{Binary: "herd", Synopsis: "all-in-one: herd-api, herd-router, herd-egress and a herdlet in one process", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			a := &AllInOne{}
			a.Flags(fs)
			return func(ctx context.Context, _ []string, _ io.Writer) error { return a.Run(ctx, nil) }
		}},
		{Binary: "herd", Sub: "conformance", Synopsis: "run the Substrate-API conformance suite against a target (Substrate or herd)", New: func(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
			o := &substrateapi.Options{}
			o.Flags(fs)
			return func(ctx context.Context, _ []string, out io.Writer) error {
				cfg, err := o.Config()
				if err != nil {
					return err
				}
				rep := substrateapi.Run(ctx, cfg)
				rep.Print(out)
				if !rep.OK() {
					return fmt.Errorf("the suite is not green on %s", rep.Target)
				}
				return nil
			}
		}},
	}
}

// Lookup finds the command typed as binary [sub].
func Lookup(binary, sub string) (Command, bool) {
	for _, c := range Commands() {
		if c.Binary == binary && c.Sub == sub {
			return c, true
		}
	}
	return Command{}, false
}

// FlagNames returns the flags a command accepts.
func (c Command) FlagNames() []string {
	fs := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	c.New(fs)
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	return names
}

func subs(binary string) []string {
	var out []string
	for _, c := range Commands() {
		if c.Binary == binary && c.Sub != "" {
			out = append(out, c.Sub)
		}
	}
	return out
}

// Runner is a parsed command line, ready to run.
type Runner struct {
	Command    Command
	Positional []string
	run        func(ctx context.Context, pos []string, out io.Writer) error
}

// ErrUsage is returned by Parse for a command line the binary would refuse.
var ErrUsage = errors.New("usage")

// Parse parses a command line of binary (its arguments after the program name) exactly as the binary does, without
// running it. Errors wrap ErrUsage (or flag.ErrHelp for -h).
func Parse(binary string, args []string, stderr io.Writer) (Runner, error) {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if _, ok := Lookup(binary, args[0]); ok {
			sub, args = args[0], args[1:]
		}
	}
	c, ok := Lookup(binary, sub)
	if !ok {
		return Runner{}, fmt.Errorf("%w: %s %s ...", ErrUsage, binary, strings.Join(subs(binary), "|"))
	}
	fs := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	fs.SetOutput(stderr)
	run := c.New(fs)
	// Positional arguments may come before the flags (herdlet join <herd-api> --token <t>) or after them.
	var pos []string
	for len(c.Positional) > len(pos) && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Runner{}, err
		}
		return Runner{}, fmt.Errorf("%w: %s: %v", ErrUsage, c.Name(), err)
	}
	pos = append(pos, fs.Args()...)
	if len(pos) > len(c.Positional) {
		return Runner{}, fmt.Errorf("%w: %s: unexpected arguments %q", ErrUsage, c.Name(), pos[len(c.Positional):])
	}
	return Runner{Command: c, Positional: pos, run: run}, nil
}

// Main runs binary with args (os.Args[1:]) and returns the exit code.
func Main(binary string, args []string) int {
	r, err := Parse(binary, args, os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := r.run(ctx, r.Positional, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", r.Command.Name(), err)
		return 1
	}
	return 0
}

func herdletRun(fs *flag.FlagSet) func(context.Context, []string, io.Writer) error {
	h := &Herdlet{}
	h.RunFlags(fs)
	return func(ctx context.Context, _ []string, _ io.Writer) error { return h.Run(ctx) }
}
