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
	MoveFrom        string
	MoveTo          string
	Prefix          string
	Wait            time.Duration
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
	fs.StringVar(&o.MoveFrom, "move-from", "", "worker selector k=v,... of the pool the move case starts on")
	fs.StringVar(&o.MoveTo, "move-to", "", "worker selector k=v,... of the pool the move case resumes on")
	fs.StringVar(&o.Prefix, "prefix", "", "prefix of every name the suite creates (default: cf-<random>)")
	fs.DurationVar(&o.Wait, "wait", 3*time.Minute, "how long each wait for a state may take")
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
		Atespace: o.Atespace, Template: o.Template, Probe: p, VolumePath: o.VolumePath, Prefix: o.Prefix, Wait: o.Wait}
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
	return cfg, nil
}
