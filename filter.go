package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Filter is a set of space-separated conditions that must all hold, e.g.
//
//	size>100M mod>1y ext:dmg,zip !path:Photos
type Filter struct {
	Raw      string
	Type     string // "", "file", "dir" or "any"
	conds    []cond
	needPath bool
}

type cond struct {
	neg bool
	fn  func(n *Node, path func() string) bool
}

type preset struct {
	key, name, expr string
}

var presets = []preset{
	{"1", "Large files", "size>100M"},
	{"2", "Old & big", "mod>1y size>50M"},
	{"3", "Installers & archives", "ext:dmg,pkg,iso,xip,zip,tar,gz,tgz,bz2,xz,7z,rar"},
	{"4", "Dev junk folders", "type:dir name:node_modules,DerivedData,.venv,venv,__pycache__,.gradle,.next,Pods,.terraform"},
	{"5", "Not opened in 6 months", "accessed>6m size>10M"},
	{"6", "Videos & disk images", "ext:mov,mp4,mkv,avi,m4v,vmdk,vdi,qcow2,img,raw,sparseimage,sparsebundle"},
}

func ParseFilter(s string) (*Filter, error) {
	f := &Filter{Raw: strings.TrimSpace(s)}
	now := time.Now()
	for _, tok := range strings.Fields(s) {
		neg := false
		if len(tok) > 1 && tok[0] == '!' {
			neg, tok = true, tok[1:]
		}
		if i := strings.IndexAny(tok, "<>"); i > 0 {
			c, err := parseCmp(strings.ToLower(tok[:i]), tok[i], strings.TrimLeft(tok[i+1:], "="), now)
			if err != nil {
				return nil, err
			}
			c.neg = neg
			f.conds = append(f.conds, c)
			continue
		}
		if i := strings.IndexByte(tok, ':'); i > 0 {
			key, val := strings.ToLower(tok[:i]), tok[i+1:]
			switch key {
			case "name", "n":
				f.conds = append(f.conds, cond{neg, nameMatcher(val)})
			case "ext", "e":
				exts := map[string]bool{}
				for _, e := range strings.Split(strings.ToLower(val), ",") {
					exts[strings.TrimPrefix(e, ".")] = true
				}
				f.conds = append(f.conds, cond{neg, func(n *Node, _ func() string) bool {
					return exts[strings.TrimPrefix(strings.ToLower(filepath.Ext(n.Name)), ".")]
				}})
			case "path", "p":
				needle := strings.ToLower(val)
				f.needPath = true
				f.conds = append(f.conds, cond{neg, func(_ *Node, path func() string) bool {
					return strings.Contains(strings.ToLower(path()), needle)
				}})
			case "type", "t":
				switch strings.ToLower(val) {
				case "f", "file", "files":
					f.Type = "file"
				case "d", "dir", "dirs", "folder", "folders":
					f.Type = "dir"
				case "a", "any", "all":
					f.Type = "any"
				default:
					return nil, fmt.Errorf("type must be file, dir or any")
				}
			default:
				return nil, fmt.Errorf("unknown filter key %q", key)
			}
			continue
		}
		f.conds = append(f.conds, cond{neg, nameMatcher(tok)})
	}
	return f, nil
}

func (f *Filter) Active() bool { return f != nil && (len(f.conds) > 0 || f.Type != "") }

func (f *Filter) Match(n *Node, path func() string) bool {
	for _, c := range f.conds {
		if c.fn(n, path) == c.neg {
			return false
		}
	}
	return true
}

// nameMatcher matches any of the comma-separated patterns: a glob if it
// contains wildcard characters, otherwise a case-insensitive substring.
func nameMatcher(val string) func(*Node, func() string) bool {
	pats := strings.Split(strings.ToLower(val), ",")
	return func(n *Node, _ func() string) bool {
		name := strings.ToLower(n.Name)
		for _, p := range pats {
			if strings.ContainsAny(p, "*?[") {
				if ok, _ := filepath.Match(p, name); ok {
					return true
				}
			} else if strings.Contains(name, p) {
				return true
			}
		}
		return false
	}
}

func parseCmp(key string, op byte, val string, now time.Time) (cond, error) {
	var get func(*Node) time.Time
	switch key {
	case "size", "s":
		limit, err := parseSize(val)
		if err != nil {
			return cond{}, err
		}
		return cond{fn: func(n *Node, _ func() string) bool {
			if op == '>' {
				return n.Size >= limit
			}
			return n.Size < limit
		}}, nil
	case "mod", "modified", "m":
		get = func(n *Node) time.Time { return n.Newest }
	case "created", "c", "born":
		get = func(n *Node) time.Time { return n.Created }
	case "accessed", "acc", "a", "opened":
		get = func(n *Node) time.Time { return n.LastAccess }
	default:
		return cond{}, fmt.Errorf("unknown filter key %q", key)
	}
	d, err := parseAge(val)
	if err != nil {
		return cond{}, err
	}
	cutoff := now.Add(-d)
	// For ages, ">" means older than and "<" means more recent than.
	return cond{fn: func(n *Node, _ func() string) bool {
		if op == '>' {
			return get(n).Before(cutoff)
		}
		return !get(n).Before(cutoff)
	}}, nil
}

func parseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(s, "B"), "b"))
	mult := 1.0
	if len(u) > 0 {
		switch u[len(u)-1] {
		case 'K':
			mult = 1e3
		case 'M':
			mult = 1e6
		case 'G':
			mult = 1e9
		case 'T':
			mult = 1e12
		}
		if mult != 1 {
			u = u[:len(u)-1]
		}
	}
	v, err := strconv.ParseFloat(u, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q (try 500M, 2G)", s)
	}
	return int64(v * mult), nil
}

func parseAge(s string) (time.Duration, error) {
	unit := time.Duration(24 * time.Hour)
	num := strings.ToLower(s)
	if len(num) > 0 {
		switch num[len(num)-1] {
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		case 'm':
			unit = 30 * 24 * time.Hour
		case 'y':
			unit = 365 * 24 * time.Hour
		}
		if num[len(num)-1] < '0' || num[len(num)-1] > '9' {
			num = num[:len(num)-1]
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("bad age %q (try 30d, 6m, 2y)", s)
	}
	return time.Duration(v * float64(unit)), nil
}

// collectFlat lists everything beneath dir that matches f. Files only by
// default; with type:dir, matching folders are listed without descending
// into them, so nested matches don't double count.
func collectFlat(dir *Node, f *Filter, showHidden bool) []*Node {
	typ := f.Type
	if typ == "" {
		typ = "file"
	}
	var out []*Node
	var walk func(n *Node, p string)
	walk = func(n *Node, p string) {
		for _, c := range n.Children {
			if !showHidden && isHidden(c.Name) {
				continue
			}
			cp := ""
			if f.needPath {
				cp = p + "/" + c.Name
			}
			path := func() string { return cp }
			if c.IsDir {
				if typ != "file" && f.Match(c, path) {
					out = append(out, c)
					continue
				}
				walk(c, cp)
			} else if typ != "dir" && f.Match(c, path) {
				out = append(out, c)
			}
		}
	}
	walk(dir, dir.Path())
	return out
}

func isHidden(name string) bool { return strings.HasPrefix(name, ".") }
