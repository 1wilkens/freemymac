package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"time"
)

type benchResult struct {
	wall, user, sys time.Duration
}

func (r benchResult) String() string {
	cpu := r.user + r.sys
	return fmt.Sprintf("%6.2fs  (cpu %5.1fs = %4.1f user + %5.1f sys, %4.1f cores busy)",
		r.wall.Seconds(), cpu.Seconds(), r.user.Seconds(), r.sys.Seconds(), cpu.Seconds()/r.wall.Seconds())
}

func timedScan(dir string, opt ScanOptions) (*Node, benchResult, error) {
	runtime.GC()
	var r0, r1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
	start := time.Now()
	p := &Progress{}
	p.Reset()
	n, err := Scan(dir, p, opt)
	wall := time.Since(start)
	syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
	return n, benchResult{
		wall: wall,
		user: time.Duration(r1.Utime.Nano() - r0.Utime.Nano()),
		sys:  time.Duration(r1.Stime.Nano() - r0.Stime.Nano()),
	}, err
}

// bench checks that the platform fast path agrees with the portable one,
// then times both across worker counts to show where the scan is bound.
func bench(dir string) int {
	dir, _ = filepath.Abs(dir)
	fmt.Printf("Scanning %s on %d CPUs (default %d workers). Takes 2-3 minutes.\n\n", dir, runtime.NumCPU(), defaultWorkers())

	warm, r, err := timedScan(dir, ScanOptions{})
	if err != nil {
		fmt.Println("scan failed:", err)
		return 1
	}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	fmt.Printf("warm-up   %s\n          %s files, %s, tree uses %d MB of memory\n\n",
		r, commas(warm.Files), human(warm.Size), ms.HeapAlloc>>20)
	warm = nil

	// Time the fast path first. The portable path pulls every file's
	// metadata into the kernel's caches, which skews whatever runs after it.
	workers := []int{2, 4, 8, 16, 32}
	if d := defaultWorkers(); !slices.Contains(workers, d) {
		workers = append(workers, d)
		slices.Sort(workers)
	}
	best := map[int]benchResult{}
	run := func(w int) {
		_, r, _ := timedScan(dir, ScanOptions{Workers: w})
		mark := ""
		if w == defaultWorkers() {
			mark = " (default)"
		}
		fmt.Printf("%4d  %s%s\n", w, r, mark)
		if b, ok := best[w]; !ok || r.wall < b.wall {
			best[w] = r
		}
	}
	fmt.Println("fast path, ascending then descending worker counts:")
	for _, w := range workers {
		run(w)
	}
	for _, w := range slices.Backward(workers) {
		run(w)
	}
	bw := workers[0]
	for w, r := range best {
		if r.wall < best[bw].wall {
			bw = w
		}
	}
	fmt.Printf("best: %d workers, %.2fs\n\n", bw, best[bw].wall.Seconds())

	fmt.Println("fast vs. portable at the default worker count, then comparing results:")
	fast, rf, _ := timedScan(dir, ScanOptions{})
	fmt.Printf("fast      %s\n", rf)
	slow, rp, err := timedScan(dir, ScanOptions{Portable: true})
	if err != nil {
		fmt.Println("portable scan failed:", err)
		return 1
	}
	fmt.Printf("portable  %s\n", rp)
	var c comparison
	c.trees(fast, slow, dir)
	c.report()
	if c.mismatches > 0 {
		return 1
	}
	return 0
}

type comparison struct {
	mismatches, changed, hardlinks int
	samples                        []string
}

func (c *comparison) mismatch(format string, args ...any) {
	c.mismatches++
	if len(c.samples) < 25 {
		c.samples = append(c.samples, fmt.Sprintf(format, args...))
	}
}

// trees compares the two scans, ignoring files that changed between them
// and which name of a hard-linked file got its size.
func (c *comparison) trees(a, b *Node, path string) {
	if !a.Modified.Equal(b.Modified) {
		c.changed++
		if a.IsDir != b.IsDir {
			return
		}
	} else if a.IsDir != b.IsDir || a.IsLink != b.IsLink {
		c.mismatch("%s: type dir=%v/%v link=%v/%v", path, a.IsDir, b.IsDir, a.IsLink, b.IsLink)
		return
	} else if !a.IsDir {
		switch {
		case a.Apparent != b.Apparent:
			c.mismatch("%s: logical size %d/%d", path, a.Apparent, b.Apparent)
		case a.HardLinked || b.HardLinked:
			if a.Size != b.Size {
				c.hardlinks++
			}
		case a.Size != b.Size:
			c.mismatch("%s: size on disk %d/%d", path, a.Size, b.Size)
		}
		if !a.Created.Equal(b.Created) {
			c.mismatch("%s: created %v/%v", path, a.Created, b.Created)
		}
	}
	bc := make(map[string]*Node, len(b.Children))
	for _, ch := range b.Children {
		bc[ch.Name] = ch
	}
	for _, ch := range a.Children {
		if o, ok := bc[ch.Name]; ok {
			c.trees(ch, o, path+"/"+ch.Name)
			delete(bc, ch.Name)
		} else {
			c.changed++ // created or deleted between the two scans
		}
	}
	c.changed += len(bc)
}

func (c *comparison) report() {
	note := fmt.Sprintf("(%d entries changed between scans, %d hard links attributed to a different name: both expected)",
		c.changed, c.hardlinks)
	if c.mismatches == 0 {
		fmt.Println("✓ fast and portable scanners agree", note)
		return
	}
	fmt.Printf("✗ %d real differences %s, first ones:\n", c.mismatches, note)
	for _, s := range c.samples {
		fmt.Println("  " + s)
	}
}
