package agent

import (
	"errors"
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

// resolve turns a request path (absolute inside the container, or relative to its workdir) into a path relative
// to the container root for os.Root, which refuses anything that would leave the root, symlinks included.
func resolve(c *container, p string) (string, error) {
	if p == "" {
		return "", status.Errorf(codes.InvalidArgument, "file path cannot be empty")
	}
	if !path.IsAbs(p) {
		p = path.Join("/", c.t.Workdir(), p)
	}
	p = path.Clean(p)
	if p == "/" {
		return ".", nil
	}
	return strings.TrimPrefix(p, "/"), nil
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
	name, err := resolve(c, req.GetPath())
	if err != nil {
		return err
	}
	flags := os.O_RDONLY
	if req.GetMode() != 0 {
		flags |= os.O_CREATE
		if err := c.root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return fileError(err, "creating parents of", req.GetPath())
		}
	}
	f, err := c.root.OpenFile(name, flags, fs.FileMode(req.GetMode())&fs.ModePerm)
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
			name, err := resolve(c, reqP)
			if err != nil {
				return nil, err
			}
			if req.GetSeekOffset() < 0 {
				return nil, status.Errorf(codes.InvalidArgument, "seek_offset cannot be negative")
			}
			if err := c.root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return nil, fileError(err, "creating parents of", reqP)
			}
			mode := fs.FileMode(0o644)
			if req.GetMode() != 0 {
				mode = fs.FileMode(req.GetMode()) & fs.ModePerm
			}
			flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			if req.GetSeekOffset() > 0 {
				flags = os.O_CREATE | os.O_WRONLY
			}
			if f, err = c.root.OpenFile(name, flags, mode); err != nil {
				return nil, fileError(err, "opening", reqP)
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

func fileInfo(c *container, name string, fi fs.FileInfo) *herdext.FileInfo {
	out := &herdext.FileInfo{
		Path:       path.Join("/", name),
		Name:       fi.Name(),
		Size:       fi.Size(),
		Mode:       uint32(fi.Mode().Perm()),
		ModifiedAt: timestamppb.New(fi.ModTime()),
	}
	if name == "." {
		out.Name = "/"
	}
	switch {
	case fi.Mode().IsRegular():
		out.Type = herdext.FileType_FILE_TYPE_REGULAR
	case fi.IsDir():
		out.Type = herdext.FileType_FILE_TYPE_DIRECTORY
	case fi.Mode()&fs.ModeSymlink != 0:
		out.Type = herdext.FileType_FILE_TYPE_SYMLINK
		if t, err := c.root.Readlink(name); err == nil {
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
	name, err := resolve(c, req.GetPath())
	if err != nil {
		return nil, err
	}
	fi, err := c.root.Lstat(name)
	if err != nil {
		return nil, fileError(err, "stat", req.GetPath())
	}
	return fileInfo(c, name, fi), nil
}

// ListDirectory describes a directory's entries, sorted by name.
func (g *Guest) ListDirectory(md func(string) string, req *herdext.ListDirectoryRequest) (*herdext.ListDirectoryResponse, error) {
	c, err := g.target(md)
	if err != nil {
		return nil, err
	}
	name, err := resolve(c, req.GetPath())
	if err != nil {
		return nil, err
	}
	d, err := c.root.Open(name)
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
		out.Entries = append(out.Entries, fileInfo(c, path.Join(name, e.Name()), fi))
	}
	return out, nil
}
