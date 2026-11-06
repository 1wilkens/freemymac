package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Well-known places where space accumulates on a Mac, relative to $HOME.
var culpritPaths = []struct{ path, desc string }{
	{".Trash", "Your Trash. Empty it in Finder to actually free the space"},
	{"Downloads", "Downloads; often full of old installers and archives"},
	{"Library/Caches", "App caches. Usually safe to clear; apps rebuild them (quit apps first)"},
	{"Library/Caches/Homebrew", "Homebrew download cache. Safe: `brew cleanup --prune=all`"},
	{"Library/Logs", "Application logs. Safe to delete"},
	{"Library/Developer/Xcode/DerivedData", "Xcode build products. Safe to delete"},
	{"Library/Developer/Xcode/Archives", "Xcode archived app builds. Keep any you need to symbolicate"},
	{"Library/Developer/Xcode/iOS DeviceSupport", "Debug symbols per iOS version. Safe; re-created on device connect"},
	{"Library/Developer/Xcode/watchOS DeviceSupport", "Debug symbols per watchOS version. Safe"},
	{"Library/Developer/CoreSimulator/Devices", "iOS simulators. Prefer `xcrun simctl delete unavailable`"},
	{"Library/Developer/CoreSimulator/Caches", "Simulator caches. Safe to delete"},
	{"Library/Application Support/MobileSync/Backup", "iPhone/iPad backups. Manage in Finder > device > Manage Backups"},
	{"Library/Containers/com.docker.docker", "Docker Desktop VM disk. Use `docker system prune` or Docker settings"},
	{"Library/Messages/Attachments", "iMessage attachments. Manage in Settings > General > Storage"},
	{"Library/Mail", "Mail.app data. Don't delete by hand"},
	{"Library/Application Support/Code/CachedExtensionVSIXs", "VS Code extension package cache. Safe"},
	{"Library/Application Support/Slack/Cache", "Slack cache. Safe"},
	{"Library/Application Support/Google/Chrome", "Chrome profile incl. caches"},
	{"Library/Group Containers/group.com.apple.CoreSimulator", "Simulator shared data"},
	{".cache", "Generic tool cache (pip, huggingface, etc.). Usually safe"},
	{".cache/huggingface", "Downloaded ML models/datasets. Re-downloadable"},
	{".npm", "npm cache. Safe: `npm cache clean --force`"},
	{".yarn/berry/cache", "Yarn cache. Safe"},
	{"Library/Caches/pnpm", "pnpm store cache"},
	{"Library/pnpm", "pnpm store. `pnpm store prune`"},
	{".gradle/caches", "Gradle cache. Safe; re-downloaded"},
	{".m2/repository", "Maven repository. Re-downloadable"},
	{"go/pkg/mod", "Go module cache. Safe: `go clean -modcache`"},
	{".cargo/registry", "Cargo registry cache. Re-downloadable"},
	{".rustup/toolchains", "Rust toolchains. Remove unused via `rustup toolchain uninstall`"},
	{".pyenv/versions", "pyenv Python versions"},
	{".nvm/versions", "nvm Node versions. `nvm uninstall <ver>`"},
	{".conda", "Conda environments/packages. `conda clean --all`"},
	{"miniconda3/pkgs", "Conda package cache. `conda clean --all`"},
	{".ollama/models", "Ollama models. `ollama rm <model>`"},
	{".lmstudio/models", "LM Studio models"},
	{".docker", "Docker CLI data"},
	{".colima", "Colima VM disks"},
	{".orbstack", "OrbStack data"},
	{".android", "Android SDK/emulator data"},
	{"Library/Android/sdk", "Android SDK"},
	{"Movies", "Movies (personal media)"},
	{"Pictures/Photos Library.photoslibrary", "Photos library. Manage inside Photos, never by hand"},
}

// Folder names that are recognisable anywhere.
var culpritNames = map[string]string{
	"node_modules":            "JS dependencies. Safe; reinstall with npm/yarn/pnpm install",
	"DerivedData":             "Xcode build products. Safe to delete",
	".venv":                   "Python virtualenv. Recreatable from requirements",
	"venv":                    "Python virtualenv. Recreatable from requirements",
	"__pycache__":             "Python bytecode cache. Safe",
	".next":                   "Next.js build cache. Safe",
	"Pods":                    "CocoaPods dependencies. Reinstall with `pod install`",
	".terraform":              "Terraform providers/modules. Re-fetched by `terraform init`",
	".gradle":                 "Gradle cache/build state",
	"Caches":                  "Cache folder. Usually safe to clear",
	".Trash":                  "Trash. Empty it to free space",
	"Docker.raw":              "Docker Desktop VM disk. Shrink via Docker settings / prune",
	"target":                  "Possibly a Rust/Maven build dir (cargo clean / mvn clean)",
	".DocumentRevisions-V100": "macOS document versions. Managed by the system",
}

type culprit struct {
	node *Node
	desc string
}

func culpritIndex(home string) map[string]string {
	idx := make(map[string]string, len(culpritPaths))
	for _, c := range culpritPaths {
		idx[filepath.Join(home, c.path)] = c.desc
	}
	return idx
}

func findCulprits(root *Node, home string) []culprit {
	var out []culprit
	for _, c := range culpritPaths {
		if n := lookup(root, filepath.Join(home, c.path)); n != nil && n.Size > 0 {
			out = append(out, culprit{n, c.desc})
		}
	}
	slices.SortFunc(out, func(a, b culprit) int { return int64Cmp(b.node.Size, a.node.Size) })
	return out
}

func lookup(root *Node, abs string) *Node {
	rel, err := filepath.Rel(root.Path(), abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil
	}
	n := root
	if rel == "." {
		return n
	}
next:
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		for _, c := range n.Children {
			if c.Name == part {
				n = c
				continue next
			}
		}
		return nil
	}
	return n
}

func int64Cmp(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
