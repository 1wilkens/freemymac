//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

func (s *scanner) list(fd int) ([]entry, int, error) { return listStat(fd) }

// Linux's stat(2) has no birth time; creation dates show as "-".
func statTimes(st *unix.Stat_t) (mod, acc, birth time.Time) {
	return ts(st.Mtim), ts(st.Atim), time.Time{}
}

func ts(t unix.Timespec) time.Time { return time.Unix(int64(t.Sec), int64(t.Nsec)) }
