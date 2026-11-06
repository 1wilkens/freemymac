package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Node is a file or directory in the scanned tree. For directories the size,
// file count and "newest" timestamps are aggregated over everything beneath.
type Node struct {
	Name     string // root node holds the full path
	Parent   *Node
	Children []*Node
	IsDir    bool
	IsLink   bool
	// HardLinked files have several names; only the first one the scan
	// reaches carries the size, the others show 0.
	HardLinked bool

	Size     int64 // bytes actually allocated on disk
	Apparent int64 // logical size (what Finder calls "size")
	Files    int64 // number of files at or beneath this node

	Created    time.Time
	Modified   time.Time // own mtime
	Accessed   time.Time // own atime
	Newest     time.Time // dirs: most recent mtime of anything inside
	LastAccess time.Time // dirs: most recent atime of any file inside

	Err string
}

func (n *Node) Path() string {
	if n.Parent == nil {
		return n.Name
	}
	return filepath.Join(n.Parent.Path(), n.Name)
}

type Progress struct {
	Files, Bytes, Errors atomic.Int64
	current              atomic.Pointer[string]
}

func (p *Progress) Reset() {
	p.Files.Store(0)
	p.Bytes.Store(0)
	p.Errors.Store(0)
	empty := ""
	p.current.Store(&empty)
}

func (p *Progress) Current() string {
	if s := p.current.Load(); s != nil {
		return *s
	}
	return ""
}

type ScanOptions struct {
	Portable bool // skip platform fast paths (used by --bench to compare)
	Workers  int  // max concurrent directory readers; 0 = default
}

// More readers than cores just queues them on kernel locks: on an 8-core
// M-series Mac, 8 was ~40% faster than 32 for a 1.6M-file home folder.
func defaultWorkers() int { return runtime.NumCPU() }

type scanner struct {
	rootDev  uint64
	portable bool
	sem      chan struct{}
	wg       sync.WaitGroup
	seen     sync.Map // hardlinked inodes already counted
	prog     *Progress
}

// entry is a directory entry as reported by a platform lister.
type entry struct {
	node  *Node
	ino   uint64
	nlink uint32
}

const otherFS = "on another filesystem (not scanned)"

// Scan walks path in parallel without following symlinks or crossing into
// other filesystems.
func Scan(path string, prog *Progress, opt ScanOptions) (*Node, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, err
	}
	root := nodeFromStat(path, &st).node
	if root.IsDir {
		if opt.Workers <= 0 {
			opt.Workers = defaultWorkers()
		}
		s := &scanner{
			rootDev:  uint64(st.Dev),
			portable: opt.Portable,
			sem:      make(chan struct{}, opt.Workers),
			prog:     prog,
		}
		s.wg.Add(1)
		s.scanDir(root, path)
		s.wg.Wait()
	}
	finalize(root)
	return root, nil
}

func (s *scanner) scanDir(n *Node, path string) {
	defer s.wg.Done()
	s.prog.current.Store(&path)
	// A raw fd rather than os.Open, which costs an extra fstat and fcntl
	// per directory; at ~200k directories that adds up.
	fd, err := openDir(path)
	if err != nil {
		n.Err = err.Error()
		s.prog.Errors.Add(1)
		return
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) == nil && uint64(st.Dev) != s.rootDev {
		unix.Close(fd)
		n.Err = otherFS
		return
	}
	ents, nerr, err := s.list(fd)
	// Close before descending so open handles don't pile up with depth.
	unix.Close(fd)
	if err != nil {
		n.Err = err.Error()
		nerr++
	}

	n.Children = make([]*Node, 0, len(ents))
	var files, bytes int64
	var subdirs []*Node
	for _, e := range ents {
		c := e.node
		c.Parent = n
		n.Children = append(n.Children, c)
		if c.IsDir {
			subdirs = append(subdirs, c)
			continue
		}
		if e.nlink > 1 {
			c.HardLinked = true
			if _, dup := s.seen.LoadOrStore(e.ino, true); dup {
				c.Size = 0
			}
		}
		files++
		bytes += c.Size
	}
	s.prog.Files.Add(files)
	s.prog.Bytes.Add(bytes)
	if nerr > 0 {
		s.prog.Errors.Add(int64(nerr))
	}

	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for _, c := range subdirs {
		p := prefix + c.Name
		s.wg.Add(1)
		select {
		case s.sem <- struct{}{}:
			go func() {
				s.scanDir(c, p)
				<-s.sem
			}()
		default:
			s.scanDir(c, p)
		}
	}
}

func openDir(path string) (int, error) {
	for {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != unix.EINTR {
			return fd, err
		}
	}
}

// listStat is the portable lister: read names, then fstatat each one
// relative to the open directory (avoids re-resolving the full path).
func listStat(fd int) ([]entry, int, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, 0, err
	}
	f := os.NewFile(uintptr(dup), "")
	names, err := f.Readdirnames(-1)
	f.Close()
	ents := make([]entry, 0, len(names))
	nerr := 0
	for _, name := range names {
		var st unix.Stat_t
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			nerr++
			continue
		}
		ents = append(ents, nodeFromStat(name, &st))
	}
	return ents, nerr, err
}

func nodeFromStat(name string, st *unix.Stat_t) entry {
	n := &Node{
		Name:     name,
		Size:     st.Blocks * 512,
		Apparent: st.Size,
	}
	n.Modified, n.Accessed, n.Created = statTimes(st)
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		n.IsDir = true
	case unix.S_IFLNK:
		n.IsLink = true
	}
	return entry{node: n, ino: uint64(st.Ino), nlink: uint32(st.Nlink)}
}

// finalize aggregates sizes, counts and timestamps bottom-up.
func finalize(n *Node) {
	n.Newest = n.Modified
	if !n.IsDir {
		n.Files = 1
		n.LastAccess = n.Accessed
		return
	}
	for _, c := range n.Children {
		finalize(c)
		n.Size += c.Size
		n.Apparent += c.Apparent
		n.Files += c.Files
		if c.Newest.After(n.Newest) {
			n.Newest = c.Newest
		}
		if c.LastAccess.After(n.LastAccess) {
			n.LastAccess = c.LastAccess
		}
	}
}

func propagate(from *Node, size, apparent, files int64) {
	for a := from; a != nil; a = a.Parent {
		a.Size += size
		a.Apparent += apparent
		a.Files += files
	}
}

// removeNode detaches n from the tree and subtracts its totals from ancestors.
func removeNode(n *Node) {
	p := n.Parent
	if p == nil {
		return
	}
	for i, c := range p.Children {
		if c == n {
			p.Children = append(p.Children[:i], p.Children[i+1:]...)
			break
		}
	}
	propagate(p, -n.Size, -n.Apparent, -n.Files)
	n.Parent = nil
}

// replaceNode swaps a freshly rescanned subtree in for old.
func replaceNode(old, fresh *Node) {
	p := old.Parent
	fresh.Parent = p
	if p == nil {
		return
	}
	fresh.Name = old.Name
	for i, c := range p.Children {
		if c == old {
			p.Children[i] = fresh
			break
		}
	}
	propagate(p, fresh.Size-old.Size, fresh.Apparent-old.Apparent, fresh.Files-old.Files)
}

func isAncestor(a, n *Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == a {
			return true
		}
	}
	return false
}
