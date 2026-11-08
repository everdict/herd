package substrateapi

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
)

// Options are `herd conformance`'s flags.
type Options struct {
	Target          string
	Endpoint        string
	TLS             bool
	CAFile          string
	TokenFile       string
	Atespace        string
	Template        string
	Router          string
	RouterTLS       bool
	RouterTokenFile string
	ServerName      string
	RouterName      string
	RouterCert      string
	RouterKey       string
	Probe           string
	ExpectFidelity  string
	VolumePath      string
	RootfsPath      string
	MoveFrom        string
	MoveTo          string
	Prefix          string
	Wait            time.Duration
	EgressWalk      string
	EchoListen      string
	EchoURL         string
	HostSide        string
	EgressBound     time.Duration
	Wget            string
	CredentialURI   string
	CredentialFile  string
	LongStream      bool
	StreamFor       time.Duration
	StreamGap       time.Duration
	Template2       string
	ContainerHeader string
	GoldenWait      time.Duration
	NoGolden        string
}

// Flags registers the flags on fs.
func (o *Options) Flags(fs *flag.FlagSet) {
	fs.StringVar(&o.Target, "target", "", "name of the target in the report (default: the endpoint)")
	fs.StringVar(&o.Endpoint, "endpoint", "", "Control API address, host:port (Substrate's ate-api-server or herd-api)")
	fs.BoolVar(&o.TLS, "tls", false, "dial the Control API with TLS")
	fs.StringVar(&o.CAFile, "ca-file", "", "CA bundle for -tls and -router-tls (default: the system roots)")
	fs.StringVar(&o.TokenFile, "token-file", "", "file holding the bearer token for the Control API")
	fs.StringVar(&o.Atespace, "atespace", "", "atespace the suite creates its actors in")
	fs.StringVar(&o.Template, "template", "", "existing ActorTemplate in -atespace every actor is created from")
	fs.StringVar(&o.Router, "router", "", "actor ingress address, host:port (atenet-router or herd-router)")
	fs.BoolVar(&o.RouterTLS, "router-tls", false, "reach the router with TLS")
	fs.StringVar(&o.RouterTokenFile, "router-token-file", "", "bearer token for the router (default: -token-file's)")
	fs.StringVar(&o.ServerName, "tls-server-name", "", "name the Control API's certificate is checked for (Substrate on a port-forward: api.ate-system.svc)")
	fs.StringVar(&o.RouterName, "router-tls-server-name", "", "name the router's certificate is checked for (Substrate: atenet-router.ate-system.svc)")
	fs.StringVar(&o.RouterCert, "router-cert-file", "", "client certificate (PEM) for the router; atenet-router requires one")
	fs.StringVar(&o.RouterKey, "router-key-file", "", "key of -router-cert-file")
	fs.StringVar(&o.Probe, "probe", "http-get:/", "request sent through the router: http-get:<path>, sandbox-process or env-exec")
	fs.StringVar(&o.ExpectFidelity, "expect-fidelity", "", "fidelity the target declared for the template's suspends: VOLUMES, ROOTFS or MEMORY (default: any up to the template's preferred_fidelity)")
	fs.StringVar(&o.VolumePath, "volume-path", "", "a file path inside one of the template's volumes; with a probe that writes files, suspend, fork and move check the volume")
	fs.StringVar(&o.RootfsPath, "rootfs-path", "", "fork-keeps-rootfs: a file path in the container's own filesystem, outside its volumes and /tmp (e.g. /root/conformance-rootfs.txt); with a probe that writes files, an actor from a tag of a running actor must read it")
	fs.StringVar(&o.MoveFrom, "move-from", "", "worker selector k=v,... of the pool the move case starts on")
	fs.StringVar(&o.MoveTo, "move-to", "", "worker selector k=v,... of the pool the move case resumes on")
	fs.StringVar(&o.Prefix, "prefix", "", "prefix of every name the suite creates (default: cf-<random>)")
	fs.DurationVar(&o.Wait, "wait", 3*time.Minute, "how long each wait for a state may take")
	fs.StringVar(&o.EgressWalk, "egress-walk", "", "egress-walk case: URL of a host the actor's EgressPolicy lists (http:// or https://); needs -echo-listen and -echo-url")
	fs.StringVar(&o.EchoListen, "echo-listen", "", "address the suite's echo server listens on (egress-walk, credential-header)")
	fs.StringVar(&o.EchoURL, "echo-url", "", "http:// URL at which a guest reaches the echo server through the target's egress: a name the gateway resolves to this host, at an address it does not count host-side")
	fs.StringVar(&o.HostSide, "host-side", "", "more URLs, comma-separated, the egress walk wants refused (besides the Control and router endpoints and the metadata targets)")
	fs.DurationVar(&o.EgressBound, "egress-bound", 15*time.Second, "how long a created or updated EgressPolicy may take to apply to the live actor")
	fs.StringVar(&o.Wget, "wget", "wget", "the guest's wget (busybox)")
	fs.StringVar(&o.CredentialURI, "credential-uri", "", "credential-header case: credential_uri the gateway resolves (e.g. ate-secret://dir/conformance/token)")
	fs.StringVar(&o.CredentialFile, "credential-file", "", "file holding the value -credential-uri resolves to, which the echo server must see")
	fs.BoolVar(&o.LongStream, "long-stream", false, "run the long-stream case")
	fs.DurationVar(&o.StreamFor, "stream-for", 30*time.Minute, "long-stream: how long the streamed exec is held open through the router")
	fs.DurationVar(&o.StreamGap, "stream-gap", time.Minute, "long-stream: the idle gap between the exec's outputs")
	fs.StringVar(&o.Template2, "template2", "", "two-containers case: a template in -atespace whose two containers mount one DurableDir volume")
	fs.DurationVar(&o.GoldenWait, "golden-wait", 2*time.Minute, "golden-template case: how long the template's golden snapshot may take (everdict-herder waits 120 s)")
	fs.StringVar(&o.NoGolden, "no-golden", "", "golden-template case: skip it, giving why the target does not serve golden snapshots")
	fs.StringVar(&o.ContainerHeader, "container-header", "", "two-containers: request metadata key the target's guest picks a container by (herd: x-herd-container)")
}

func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func selector(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("selector %q: want k=v,k=v", s)
		}
		out[k] = v
	}
	return out, nil
}

// Config turns the flags into a suite configuration.
func (o *Options) Config() (Config, error) {
	if o.Endpoint == "" || o.Atespace == "" || o.Template == "" || o.Router == "" {
		return Config{}, errors.New("-endpoint, -atespace, -template and -router are required")
	}
	tok, err := readToken(o.TokenFile)
	if err != nil {
		return Config{}, err
	}
	rtok := tok
	if o.RouterTokenFile != "" {
		if rtok, err = readToken(o.RouterTokenFile); err != nil {
			return Config{}, err
		}
	}
	p, err := ParseProbe(o.Probe)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Target: o.Target, Control: Endpoint{Addr: o.Endpoint, TLS: o.TLS, CAFile: o.CAFile, ServerName: o.ServerName}, Token: tok,
		Router: Router{Endpoint: Endpoint{Addr: o.Router, TLS: o.RouterTLS, CAFile: o.CAFile, ServerName: o.RouterName,
			CertFile: o.RouterCert, KeyFile: o.RouterKey}, Token: rtok},
		Atespace: o.Atespace, Template: o.Template, Probe: p, VolumePath: o.VolumePath, RootfsPath: o.RootfsPath, Prefix: o.Prefix, Wait: o.Wait}
	if o.ExpectFidelity != "" {
		f, ok := ateapi.SnapshotFidelity_value["SNAPSHOT_FIDELITY_"+strings.ToUpper(o.ExpectFidelity)]
		if !ok || f == 0 {
			return Config{}, fmt.Errorf("-expect-fidelity %q: want VOLUMES, ROOTFS or MEMORY", o.ExpectFidelity)
		}
		cfg.ExpectFidelity = ateapi.SnapshotFidelity(f)
	}
	if cfg.MoveFrom, err = selector(o.MoveFrom); err != nil {
		return Config{}, err
	}
	if cfg.MoveTo, err = selector(o.MoveTo); err != nil {
		return Config{}, err
	}
	if (cfg.MoveFrom == nil) != (cfg.MoveTo == nil) {
		return Config{}, errors.New("-move-from and -move-to go together")
	}
	cfg.EgressListed, cfg.EchoListen, cfg.EchoURL, cfg.EgressBound, cfg.Wget = o.EgressWalk, o.EchoListen, o.EchoURL, o.EgressBound, o.Wget
	if o.HostSide != "" {
		cfg.HostSide = strings.Split(o.HostSide, ",")
	}
	cfg.CredentialURI = o.CredentialURI
	if o.CredentialFile != "" {
		b, err := os.ReadFile(o.CredentialFile)
		if err != nil {
			return Config{}, err
		}
		cfg.Credential = strings.TrimRight(string(b), "\r\n")
	}
	cfg.LongStream, cfg.StreamFor, cfg.StreamGap = o.LongStream, o.StreamFor, o.StreamGap
	cfg.Template2, cfg.ContainerHeader = o.Template2, o.ContainerHeader
	cfg.GoldenWait, cfg.NoGolden = o.GoldenWait, o.NoGolden
	return cfg, nil
}
