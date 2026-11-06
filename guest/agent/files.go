package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ReadBufferSize is the chunk size of ReadFile, as agent-substrate/env's guest uses.
const ReadBufferSize = 64 * 1024

// resolve turns a request path (absolute inside the container, or relative to its workdir) into the os.Root it lies
// in and a path relative to that root; os.Root refuses anything that would leave it, symlinks included. A path at
// or below a volume's mount path lies in the volume's directory, as the container's processes see it. cpath is the
// clean absolute path in the container.
func resolve(c *container, p string) (root *os.Root, name, cpath string, err error) {
	if p == "" {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "file path cannot be empty")
	}
	if !path.IsAbs(p) {
		p = path.Join("/", c.t.Workdir(), p)
	}
	p = path.Clean(p)
	for _, v := range c.vols {
		if p == v.path {
			return v.root, ".", p, nil
		}
		if rest, ok := strings.CutPrefix(p, v.path+"/"); ok {
			return v.root, rest, p, nil
		}
	}
	if p == "/" {
		return c.root, ".", p, nil
	}
	return c.root, strings.TrimPrefix(p, "/"), p, nil
}

// openVolumes opens the target's volume directories, longest mount path first so a nested mount wins.
func (c *container) openVolumes() error {
	vt, ok := c.t.(VolumeTarget)
	if !ok {
		return nil
	}
	for at, dir := range vt.VolumeDirs() {
		if !path.IsAbs(at) || path.Clean(at) != at || at == "/" {
			return fmt.Errorf("agent: container %q: volume mount path %q is not a clean absolute path", c.t.Name(), at)
		}
		r, err := os.OpenRoot(dir)
		if err != nil {
			return fmt.Errorf("agent: container %q: volume at %s: %w", c.t.Name(), at, err)
		}
		c.vols = append(c.vols, volume{path: at, root: r})
	}
	sort.Slice(c.vols, func(i, j int) bool { return len(c.vols[i].path) > len(c.vols[j].path) })
	return nil
}

// missingParents answers the directories of dir, outermost first, that do not exist yet.
func missingParents(root *os.Root, dir string) []string {
	var out []string
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if _, err := root.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			break
		}
		out = append([]string{d}, out...)
	}
	return out
}

// ownAll gives each path to the container's user: the guest writes as root, and a process must be able to edit what the
// API created for it, and remove it from the sticky /tmp.
func ownAll(names []string, chown func(string) error) error {
	for _, n := range names {
		if err := chown(n); err != nil {
			return err
		}
	}
	return nil
}

func fileError(err error, verb, p string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Errorf(codes.NotFound, "file %q not found", p)
	case errors.Is(err, fs.ErrPermission):
		return status.Errorf(codes.PermissionDenied, "permission denied %s %q", verb, p)
	case strings.Contains(err.Error(), "escapes"):
		return status.Errorf(codes.PermissionDenied, "access denied: path %q is outside the container", p)
	}
	return status.Errorf(codes.Internal, "%s %q: %v", verb, p, err)
}

// ReadFile streams a file in chunks. With a mode, a missing file is created empty (parents included).
func (g *Guest) ReadFile(md func(string) string, req *ateenv.ReadFileRequest, send func(*ateenv.ReadFileResponse) error) error {
	c, err := g.target(md)
	if err != nil {
		return err
	}
	root, name, _, err := resolve(c, req.GetPath())
	if err != nil {
		return err
	}
	flags := os.O_RDONLY
	if req.GetMode() != 0 {
		flags |= os.O_CREATE
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return fileError(err, "creating parents of", req.GetPath())
		}
	}
	f, err := root.OpenFile(name, flags, fs.FileMode(req.GetMode())&fs.ModePerm)
	if err != nil {
		return fileError(err, "reading", req.GetPath())
	}
	defer f.Close()
	buf := make([]byte, ReadBufferSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := send(&ateenv.ReadFileResponse{Chunk: buf[:n]}); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return fileError(rerr, "reading", req.GetPath())
		}
	}
}

// WriteFile writes a stream of chunks; path, mode and seek_offset come from the first message.
func (g *Guest) WriteFile(md func(string) string, recv func() (*ateenv.WriteFileRequest, error)) (*ateenv.WriteFileResponse, error) {
	c, err := g.target(md)
	if err != nil {
		return nil, err
	}
	var (
		f     *os.File
		reqP  string
		total int64
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for {
		req, err := recv()
		if errors.Is(err, io.EOF) {
			if f == nil {
				return nil, status.Errorf(codes.InvalidArgument, "no file data received")
			}
			err := f.Close()
			f = nil
			if err != nil {
				return nil, fileError(err, "closing", reqP)
			}
			return &ateenv.WriteFileResponse{BytesWritten: total}, nil
		}
		if err != nil {
			return nil, err
		}
		if f == nil {
			reqP = req.GetPath()
			root, name, _, err := resolve(c, reqP)
			if err != nil {
				return nil, err
			}
			if req.GetSeekOffset() < 0 {
				return nil, status.Errorf(codes.InvalidArgument, "seek_offset cannot be negative")
			}
			created := missingParents(root, path.Dir(name))
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return nil, fileError(err, "creating parents of", reqP)
			}
			if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
				created = append(created, name)
			}
			mode := fs.FileMode(0o644)
			if req.GetMode() != 0 {
				mode = fs.FileMode(req.GetMode()) & fs.ModePerm
			}
			flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			if req.GetSeekOffset() > 0 {
				flags = os.O_CREATE | os.O_WRONLY
			}
			if f, err = root.OpenFile(name, flags, mode); err != nil {
				return nil, fileError(err, "opening", reqP)
			}
			if ot, ok := c.t.(OwnedTarget); ok {
				if uid, gid := ot.Owner(); uid != 0 || gid != 0 {
					if err := ownAll(created, func(n string) error { return root.Chown(n, uid, gid) }); err != nil {
						return nil, fileError(err, "owning", reqP)
					}
				}
			}
			if req.GetSeekOffset() > 0 {
				if _, err := f.Seek(req.GetSeekOffset(), io.SeekStart); err != nil {
					return nil, fileError(err, "seeking in", reqP)
				}
			}
		}
		if len(req.GetChunk()) > 0 {
			n, err := f.Write(req.GetChunk())
			total += int64(n)
			if err != nil {
				return nil, fileError(err, "writing", reqP)
			}
		}
	}
}

// fileInfo describes the entry name of root, which the container sees at cpath.
func fileInfo(root *os.Root, name, cpath string, fi fs.FileInfo) *herdext.FileInfo {
	out := &herdext.FileInfo{
		Path:       cpath,
		Name:       fi.Name(),
		Size:       fi.Size(),
		Mode:       uint32(fi.Mode().Perm()),
		ModifiedAt: timestamppb.New(fi.ModTime()),
	}
	if name == "." {
		out.Name = path.Base(cpath) // "/" for the container root, the mount point's name for a volume's
	}
	switch {
	case fi.Mode().IsRegular():
		out.Type = herdext.FileType_FILE_TYPE_REGULAR
	case fi.IsDir():
		out.Type = herdext.FileType_FILE_TYPE_DIRECTORY
	case fi.Mode()&fs.ModeSymlink != 0:
		out.Type = herdext.FileType_FILE_TYPE_SYMLINK
		if t, err := root.Readlink(name); err == nil {
			out.LinkTarget = t
		}
	default:
		out.Type = herdext.FileType_FILE_TYPE_OTHER
	}
	return out
}

// StatFile describes one path without following a final symlink.
func (g *Guest) StatFile(md func(string) string, req *herdext.StatFileRequest) (*herdext.FileInfo, error) {
	c, err := g.target(md)
	if err != nil {
		return nil, err
	}
	root, name, cpath, err := resolve(c, req.GetPath())
	if err != nil {
		return nil, err
	}
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, fileError(err, "stat", req.GetPath())
	}
	return fileInfo(root, name, cpath, fi), nil
}

// ListDirectory describes a directory's entries, sorted by name.
func (g *Guest) ListDirectory(md func(string) string, req *herdext.ListDirectoryRequest) (*herdext.ListDirectoryResponse, error) {
	c, err := g.target(md)
	if err != nil {
		return nil, err
	}
	root, name, cpath, err := resolve(c, req.GetPath())
	if err != nil {
		return nil, err
	}
	d, err := root.Open(name)
	if err != nil {
		return nil, fileError(err, "listing", req.GetPath())
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, fileError(err, "listing", req.GetPath())
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	out := &herdext.ListDirectoryResponse{}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue // removed while listing
		}
		out.Entries = append(out.Entries, fileInfo(root, path.Join(name, e.Name()), path.Join(cpath, e.Name()), fi))
	}
	return out, nil
}
