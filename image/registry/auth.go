package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/everdict/herd/image"
)

// Credential is a registry login: a user and password, or an identity token (an OAuth2 refresh token the registry's
// token service takes in place of the password). It prints redacted, so a %v of it logs no secret.
type Credential struct {
	Username, Password string
	IdentityToken      string
}

func (c Credential) String() string {
	if c.IdentityToken != "" {
		return "registry credential (identity token, redacted)"
	}
	return fmt.Sprintf("registry credential (user %q, password redacted)", c.Username)
}

// GoString keeps %#v redacted too.
func (c Credential) GoString() string { return c.String() }

// Credentials are the node's registry logins (PLT-222): what a pull logs in to a registry host with. Substrate's API
// carries no pull secret, so, as a Kubernetes node's container runtime does, the node holds them.
type Credentials interface {
	// Lookup returns the login for a registry host (image.Ref.Registry); ok is false when there is none, and the
	// pull is anonymous. An error never carries the secret.
	Lookup(host string) (c Credential, ok bool, err error)
}

// DockerConfig is a docker config file's `auths` (~/.docker/config.json, a Kubernetes dockerconfigjson secret):
// per registry host, `auth` (base64 user:password), or `username` and `password`, and optionally `identitytoken`.
// The file is read at every Lookup, so a changed file is used from the next pull on without a restart; a missing or
// empty file has no logins (anonymous pulls). credsStore and credHelpers are not read.
type DockerConfig struct {
	Path string
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// read parses the file: nil without one.
func (d DockerConfig) read() (map[string]Credential, error) {
	b, err := os.ReadFile(d.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry credentials: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var f struct {
		Auths map[string]dockerAuth `json:"auths"`
	}
	// A JSON error is not wrapped: its text could quote the file.
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("registry credentials %s: not a docker config file (want {\"auths\": {...}})", d.Path)
	}
	out := map[string]Credential{}
	for key, a := range f.Auths {
		host := normalHost(key)
		c := Credential{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Auth))
			if err != nil {
				return nil, fmt.Errorf("registry credentials %s: auths[%q].auth is not base64", d.Path, key)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("registry credentials %s: auths[%q].auth is not base64 of user:password", d.Path, key)
			}
			c.Username, c.Password = user, pass
		}
		if c.IdentityToken == "" && (c.Username == "" || c.Password == "") {
			return nil, fmt.Errorf("registry credentials %s: auths[%q] has no auth, username and password, or identitytoken", d.Path, key)
		}
		out[host] = c
	}
	return out, nil
}

// Lookup reads the file and returns host's login.
func (d DockerConfig) Lookup(host string) (Credential, bool, error) {
	m, err := d.read()
	if err != nil {
		return Credential{}, false, err
	}
	c, ok := m[normalHost(host)]
	return c, ok, nil
}

// Hosts lists the registry hosts the file holds logins for (none without the file), for `herdlet doctor`.
func (d DockerConfig) Hosts() ([]string, error) {
	m, err := d.read()
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(m))
	for h := range m {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// normalHost is a docker config key or a registry host as one name: no scheme or path, lower case, and Docker Hub's
// names (index.docker.io/v1/, docker.io) as image.DefaultRegistry.
func normalHost(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	switch s {
	case "docker.io", "index.docker.io", "registry.hub.docker.com":
		return image.DefaultRegistry
	}
	return s
}
