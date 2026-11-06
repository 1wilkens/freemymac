package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// moveToTrash asks Finder to trash the paths (so "Put Back" works), falling
// back to renaming into ~/.Trash if Finder can't be scripted.
func moveToTrash(paths []string) error {
	if runtime.GOOS == "darwin" {
		script := []string{
			"-e", "on run argv",
			"-e", "set l to {}",
			"-e", "repeat with p in argv",
			"-e", "set end of l to (POSIX file (contents of p)) as alias",
			"-e", "end repeat",
			"-e", `tell application "Finder" to delete l`,
			"-e", "end run",
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if exec.CommandContext(ctx, "osascript", append(script, paths...)...).Run() == nil {
			return nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trash := filepath.Join(home, ".Trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	var errs []error
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			continue // Finder may have already handled it
		}
		dst := filepath.Join(trash, filepath.Base(p))
		if _, err := os.Lstat(dst); err == nil {
			ext := filepath.Ext(dst)
			dst = fmt.Sprintf("%s %s%s", dst[:len(dst)-len(ext)], time.Now().Format("15.04.05.000"), ext)
		}
		if err := os.Rename(p, dst); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func deletePermanently(paths []string) error {
	var errs []error
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func reveal(path string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("reveal in Finder only works on macOS")
	}
	return exec.Command("open", "-R", path).Start()
}

func diskUsage(path string) (free, total uint64) {
	var s syscall.Statfs_t
	if syscall.Statfs(path, &s) != nil {
		return 0, 0
	}
	return s.Bavail * uint64(s.Bsize), s.Blocks * uint64(s.Bsize)
}
