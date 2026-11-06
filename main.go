package main

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	home, _ := os.UserHomeDir()
	target := home
	if len(os.Args) > 1 {
		if a := os.Args[1]; a == "-h" || a == "--help" {
			fmt.Println("usage: freemymac [folder]          (default: your home folder)")
			fmt.Println("       freemymac --bench [folder]  compare fast vs. portable scanner")
			fmt.Println("\nPress ? inside the app for keys and filter syntax.")
			return
		}
		if os.Args[1] == "--bench" {
			dir := home
			if len(os.Args) > 2 {
				dir = os.Args[2]
			}
			os.Exit(bench(dir))
		}
		target = os.Args[1]
	}
	abs, err := filepath.Abs(target)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Stat(abs); err == nil && !fi.IsDir() {
			err = fmt.Errorf("%s is not a folder", abs)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "freemymac:", err)
		os.Exit(1)
	}
	m := newModel(abs, home)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "freemymac:", err)
		os.Exit(1)
	}
	if m.root == nil && m.status != "" {
		fmt.Fprintln(os.Stderr, m.status)
		os.Exit(1)
	}
}
