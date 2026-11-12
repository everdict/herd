package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Source is a process's own SPIFFE identity and the trust bundles it verifies peers with, kept current (ADR 0001
// A-60): the SPIFFE Workload API's stream by default (WorkloadAPI), files read again when they change where a cluster
// has no Workload API (Files), or a fixed set (Static: herd all-in-one's local CA, tests).
type Source interface {
	GetX509SVID() (*x509svid.SVID, error)
	GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error)
	GetJWTBundleForTrustDomain(spiffeid.TrustDomain) (*jwtbundle.Bundle, error)
}

// Self is src's current SPIFFE ID.
func Self(src Source) (spiffeid.ID, error) {
	s, err := src.GetX509SVID()
	if err != nil {
		return spiffeid.ID{}, err
	}
	return s.ID, nil
}

// DefaultWorkloadAPI is the Workload API address when SPIFFE_ENDPOINT_SOCKET is not set: a SPIRE agent's socket.
const DefaultWorkloadAPI = "unix:///run/spire/agent/sockets/api.sock"

// WorkloadAPIAddr is the Workload API address: SPIFFE_ENDPOINT_SOCKET, else DefaultWorkloadAPI.
func WorkloadAPIAddr() string {
	if a := os.Getenv("SPIFFE_ENDPOINT_SOCKET"); a != "" {
		return a
	}
	return DefaultWorkloadAPI
}

// WorkloadSource is the Workload API's stream: the process's X.509-SVID and the X.509 and JWT bundles, rotated as the
// agent sends them.
type WorkloadSource struct {
	client  *workloadapi.Client
	x509    *workloadapi.X509Source
	bundles *workloadapi.BundleSource
}

// WorkloadAPI connects to the Workload API at addr and waits up to wait for its first SVID and bundles. An agent that
// is not there, or does not answer in time, is an error: the caller refuses to start (fail closed).
func WorkloadAPI(ctx context.Context, addr string, wait time.Duration) (*WorkloadSource, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c, err := workloadapi.New(ctx, workloadapi.WithAddr(addr))
	if err != nil {
		return nil, fmt.Errorf("the SPIFFE Workload API at %s: %w", addr, err)
	}
	s := &WorkloadSource{client: c}
	if s.x509, err = workloadapi.NewX509Source(ctx, workloadapi.WithClient(c)); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the SPIFFE Workload API at %s gave no X.509-SVID within %s: %w", addr, wait, err)
	}
	if s.bundles, err = workloadapi.NewBundleSource(ctx, workloadapi.WithClient(c)); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("the SPIFFE Workload API at %s gave no trust bundles within %s: %w", addr, wait, err)
	}
	return s, nil
}

func (s *WorkloadSource) GetX509SVID() (*x509svid.SVID, error) { return s.x509.GetX509SVID() }

func (s *WorkloadSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.bundles.GetX509BundleForTrustDomain(td)
}

func (s *WorkloadSource) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return s.bundles.GetJWTBundleForTrustDomain(td)
}

// Close ends the streams.
func (s *WorkloadSource) Close() error {
	var errs []error
	if s.bundles != nil {
		errs = append(errs, s.bundles.Close())
	}
	if s.x509 != nil {
		errs = append(errs, s.x509.Close())
	}
	return errors.Join(append(errs, s.client.Close())...)
}

// FileSource is an SVID and bundles read from files (spiffe-helper's output, or what an operator writes where a
// cluster has no Workload API), each read again when it changes; a change that does not load keeps the last good
// value, logged.
type FileSource struct {
	svid    *watched[*x509svid.SVID]
	bundles *watched[bundles]
}

// Files reads the X.509-SVID (certFile: the chain, leaf first; keyFile: its PKCS#8 key) and the bundle (bundleFile:
// PEM or a SPIFFE bundle JWKS; jwtBundleFile, optional: a JWKS beside a PEM bundle), all in the SVID's trust domain.
func Files(certFile, keyFile, bundleFile, jwtBundleFile string) (*FileSource, error) {
	if certFile == "" || keyFile == "" || bundleFile == "" {
		return nil, errors.New("auth: an SVID from files needs the certificate, its key and the trust bundle")
	}
	sv, err := watch("X.509-SVID", []string{certFile, keyFile}, func([][]byte) (*x509svid.SVID, error) { return x509svid.Load(certFile, keyFile) })
	if err != nil {
		return nil, err
	}
	td := sv.get().ID.TrustDomain()
	paths := []string{bundleFile}
	if jwtBundleFile != "" {
		paths = append(paths, jwtBundleFile)
	}
	b, err := watch("bundle", paths, func(f [][]byte) (bundles, error) { return parseBundles(td, f) })
	if err != nil {
		return nil, err
	}
	return &FileSource{svid: sv, bundles: b}, nil
}

func (f *FileSource) GetX509SVID() (*x509svid.SVID, error) { return f.svid.get(), nil }

func (f *FileSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return f.bundles.get().GetX509BundleForTrustDomain(td)
}

func (f *FileSource) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return f.bundles.get().GetJWTBundleForTrustDomain(td)
}

// Static is a fixed SVID and bundles.
type Static struct {
	SVID *x509svid.SVID
	X509 *x509bundle.Bundle
	JWT  *jwtbundle.Bundle
}

func (s Static) GetX509SVID() (*x509svid.SVID, error) { return s.SVID, nil }

func (s Static) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return bundles{x509: s.X509, jwt: s.JWT}.GetX509BundleForTrustDomain(td)
}

func (s Static) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return bundles{x509: s.X509, jwt: s.JWT}.GetJWTBundleForTrustDomain(td)
}

// onlyDomain answers src's bundles for td alone: one trust domain per install (spec E2), so an SVID of a federated
// domain the Workload API also streams is foreign here.
type onlyDomain struct {
	td  spiffeid.TrustDomain
	src Source
}

func (o onlyDomain) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != o.td {
		return nil, fmt.Errorf("foreign trust domain %q", td)
	}
	return o.src.GetX509BundleForTrustDomain(td)
}

func (o onlyDomain) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	if td != o.td {
		return nil, fmt.Errorf("foreign trust domain %q", td)
	}
	return o.src.GetJWTBundleForTrustDomain(td)
}
