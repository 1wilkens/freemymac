//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getattrlistbulk(2) returns names, types, sizes and timestamps for many
// directory entries per call, instead of one lstat per file. x/sys/unix has
// no wrapper, so call libc the same way x/sys does (see bulk_darwin.s).

//go:cgo_import_dynamic libc_getattrlistbulk getattrlistbulk "/usr/lib/libSystem.B.dylib"

var libc_getattrlistbulk_trampoline_addr uintptr

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err unix.Errno)

const (
	vDir = 2 // fsobj_type_t values from <sys/vnode.h>
	vLnk = 5

	bulkCommon = unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_ERROR | unix.ATTR_CMN_NAME |
		unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME |
		unix.ATTR_CMN_ACCTIME | unix.ATTR_CMN_FILEID
	bulkFile = unix.ATTR_FILE_LINKCOUNT | unix.ATTR_FILE_ALLOCSIZE | unix.ATTR_FILE_DATALENGTH
)

var (
	errBulkUnsupported = errors.New("getattrlistbulk not supported here")
	errBulkMalformed   = errors.New("unexpected getattrlistbulk result")
	bulkBufs           = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}
)

func (s *scanner) list(fd int) ([]entry, int, error) {
	if !s.portable {
		ents, nerr, err := listBulk(fd)
		if err != errBulkUnsupported {
			return ents, nerr, err
		}
		// Some filesystems (SMB, FUSE, …) don't implement it; start over.
		if _, err := unix.Seek(fd, 0, 0); err != nil {
			return nil, 0, err
		}
	}
	return listStat(fd)
}

func listBulk(fd int) ([]entry, int, error) {
	al := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  bulkCommon,
		Fileattr:    bulkFile,
	}
	bp := bulkBufs.Get().(*[]byte)
	defer bulkBufs.Put(bp)
	buf := *bp

	var out []entry
	nerr := 0
	for calls := 0; ; {
		r, _, e := syscall_syscall6(libc_getattrlistbulk_trampoline_addr,
			uintptr(fd), uintptr(unsafe.Pointer(&al)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e == unix.EINTR {
			continue
		}
		first := calls == 0
		calls++
		if e != 0 {
			if first && (e == unix.ENOTSUP || e == unix.EINVAL) {
				return nil, 0, errBulkUnsupported
			}
			return out, nerr, e
		}
		count := int(int32(r))
		if count <= 0 {
			return out, nerr, nil
		}
		b := buf
		for range count {
			ent, size, ok, failed := parseBulkEntry(b)
			if size == 0 {
				if first && len(out) == 0 {
					return nil, 0, errBulkUnsupported
				}
				return out, nerr, errBulkMalformed
			}
			b = b[size:]
			switch {
			case failed:
				nerr++
			case ok:
				out = append(out, ent)
			}
		}
	}
}

// parseBulkEntry decodes one packed entry. Attributes appear in bit order
// (except RETURNED_ATTRS then ERROR, which always come first) and only if the
// kernel marked them as returned. size is 0 if the entry is malformed.
func parseBulkEntry(b []byte) (ent entry, size int, ok, failed bool) {
	if len(b) < 4 {
		return
	}
	length := int(binary.LittleEndian.Uint32(b))
	if length < 24 || length > len(b) {
		return
	}
	r := reader{b: b[:length], p: 4}
	common := r.u32()
	r.u32() // volattr
	r.u32() // dirattr
	file := r.u32()
	r.u32() // forkattr

	if common&unix.ATTR_CMN_ERROR != 0 && r.u32() != 0 {
		return entry{}, length, false, true
	}
	n := &Node{}
	if common&unix.ATTR_CMN_NAME != 0 {
		base := r.p
		off, l := int(int32(r.u32())), int(r.u32())
		start, end := base+off, base+off+l
		if r.bad || start < 0 || end > length || start > end {
			return
		}
		name := r.b[start:end]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		n.Name = string(name)
	}
	var typ uint32
	if common&unix.ATTR_CMN_OBJTYPE != 0 {
		typ = r.u32()
	}
	if common&unix.ATTR_CMN_CRTIME != 0 {
		n.Created = r.ts()
	}
	if common&unix.ATTR_CMN_MODTIME != 0 {
		n.Modified = r.ts()
	}
	if common&unix.ATTR_CMN_ACCTIME != 0 {
		n.Accessed = r.ts()
	}
	if common&unix.ATTR_CMN_FILEID != 0 {
		ent.ino = r.u64()
	}
	if file&unix.ATTR_FILE_LINKCOUNT != 0 {
		ent.nlink = r.u32()
	}
	if file&unix.ATTR_FILE_ALLOCSIZE != 0 {
		n.Size = int64(r.u64())
	}
	if file&unix.ATTR_FILE_DATALENGTH != 0 {
		n.Apparent = int64(r.u64())
	}
	if r.bad || n.Name == "" || n.Name == "." || n.Name == ".." {
		return entry{}, 0, false, false
	}
	n.IsDir = typ == vDir
	n.IsLink = typ == vLnk
	ent.node = n
	return ent, length, true, false
}

type reader struct {
	b   []byte
	p   int
	bad bool
}

func (r *reader) u32() uint32 {
	if r.p+4 > len(r.b) {
		r.bad = true
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.p:])
	r.p += 4
	return v
}

func (r *reader) u64() uint64 {
	if r.p+8 > len(r.b) {
		r.bad = true
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.p:])
	r.p += 8
	return v
}

func (r *reader) ts() time.Time {
	sec, nsec := r.u64(), r.u64()
	return time.Unix(int64(sec), int64(nsec))
}

func statTimes(st *unix.Stat_t) (mod, acc, birth time.Time) {
	return ts(st.Mtim), ts(st.Atim), ts(st.Btim)
}

func ts(t unix.Timespec) time.Time { return time.Unix(int64(t.Sec), int64(t.Nsec)) }
