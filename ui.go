package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

type mode int

const (
	modeBrowse mode = iota
	modeFilter
	modeConfirm
	modeCulprits
	modeHelp
)

type sortKey int

const (
	sortSize sortKey = iota
	sortModified
	sortCreated
	sortAccessed
	sortName
	sortItems
)

var sortNames = []string{"size", "modified", "created", "accessed", "name", "items"}

type (
	tickMsg     struct{}
	scanDoneMsg struct {
		node, target *Node
		err          error
		took         time.Duration
	}
	deleteDoneMsg struct {
		removed   []*Node
		freed     int64
		err       error
		permanent bool
	}
)

type model struct {
	home     string
	rootPath string
	root     *Node
	cur      *Node
	prog     *Progress
	scanning bool
	scanFrom time.Time
	busy     string
	frame    int

	flat       bool
	rows       []*Node
	cursor     int
	offset     int
	sortBy     sortKey
	reverse    bool
	filter     *Filter
	showHidden bool
	marked     map[*Node]bool
	memo       map[*Node]*Node // last selected child per directory

	mode       mode
	input      textinput.Model
	targets    []*Node
	permanent  bool
	culprits   []culprit
	culpritSel int
	hints      map[string]string

	status    string
	statusErr bool
	w, h      int
	diskFree  uint64
	diskTotal uint64
}

func newModel(root, home string) *model {
	in := textinput.New()
	in.Prompt = "filter: "
	in.Placeholder = "e.g. size>100M mod>1y ext:dmg,zip !path:Photos"
	f, _ := ParseFilter("")
	m := &model{
		home:       home,
		rootPath:   root,
		prog:       &Progress{},
		filter:     f,
		showHidden: true,
		marked:     map[*Node]bool{},
		memo:       map[*Node]*Node{},
		input:      in,
		hints:      culpritIndex(home),
	}
	m.diskFree, m.diskTotal = diskUsage(root)
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.startScan(m.rootPath, nil), tick())
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) startScan(path string, target *Node) tea.Cmd {
	m.prog.Reset()
	m.scanning = true
	m.scanFrom = time.Now()
	prog := m.prog
	return func() tea.Msg {
		start := time.Now()
		n, err := Scan(path, prog, ScanOptions{})
		return scanDoneMsg{n, target, err, time.Since(start)}
	}
}

// ---------------------------------------------------------------- update

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clamp()
		return m, nil

	case tickMsg:
		m.frame++
		if m.scanning || m.busy != "" {
			return m, tick()
		}
		return m, nil

	case scanDoneMsg:
		m.scanning = false
		if msg.err != nil {
			if m.root == nil {
				m.status = "scan failed: " + msg.err.Error()
				return m, tea.Quit
			}
			m.setErr("rescan failed: " + msg.err.Error())
			return m, nil
		}
		if msg.target == nil {
			m.root, m.cur = msg.node, msg.node
		} else {
			replaceNode(msg.target, msg.node)
			if msg.target == m.root {
				m.root = msg.node
			}
			if m.cur == msg.target {
				m.cur = msg.node
			}
		}
		clear(m.marked)
		m.diskFree, m.diskTotal = diskUsage(m.rootPath)
		m.setStatus(fmt.Sprintf("Scanned %s files (%s) in %s, %d unreadable",
			commas(m.prog.Files.Load()), human(m.prog.Bytes.Load()),
			msg.took.Round(100*time.Millisecond), m.prog.Errors.Load()))
		m.rebuild()
		return m, nil

	case deleteDoneMsg:
		m.busy = ""
		for _, n := range msg.removed {
			removeNode(n)
			delete(m.marked, n)
		}
		clear(m.marked)
		m.diskFree, m.diskTotal = diskUsage(m.rootPath)
		verb := "Moved to Trash"
		if msg.permanent {
			verb = "Deleted"
		}
		if msg.err != nil {
			m.setErr(fmt.Sprintf("%s %d item(s), %s; errors: %v", verb, len(msg.removed), human(msg.freed), msg.err))
		} else {
			note := ""
			if !msg.permanent {
				note = " (empty the Trash to reclaim it)"
			}
			m.setStatus(fmt.Sprintf("%s %d item(s), %s%s", verb, len(msg.removed), human(msg.freed), note))
		}
		m.rebuild()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.root == nil || (m.scanning && m.mode != modeHelp) {
			if msg.String() == "q" {
				return m, tea.Quit
			}
			return m, nil
		}
		switch m.mode {
		case modeFilter:
			return m.updateFilter(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeCulprits:
			return m.updateCulprits(msg)
		case modeHelp:
			m.mode = modeBrowse
			return m, nil
		}
		return m.updateBrowse(msg)
	}
	return m, nil
}

func (m *model) updateBrowse(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.status = ""
	sel := m.selected()
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup", "ctrl+b":
		m.cursor -= m.listHeight()
	case "pgdown", "ctrl+f":
		m.cursor += m.listHeight()
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.rows) - 1
	case "enter", "right", "l":
		if sel == nil {
			break
		}
		if sel.IsDir {
			m.enter(sel, false)
		} else if m.flat {
			// jump to the file's folder in tree view
			m.memo[sel.Parent] = sel
			m.enter(sel.Parent, false)
		}
		return m, nil
	case "backspace", "left", "h":
		if m.cur.Parent != nil {
			m.memo[m.cur.Parent] = m.cur
			m.enter(m.cur.Parent, m.flat)
		}
		return m, nil
	case "f":
		m.flat = !m.flat
		m.cursor = 0
		m.rebuild()
	case "/":
		m.mode = modeFilter
		m.input.SetValue(m.filter.Raw)
		m.input.CursorEnd()
		return m, m.input.Focus()
	case "esc":
		if m.filter.Active() {
			m.applyFilter("")
		} else if m.flat {
			m.flat = false
			m.rebuild()
		}
	case "0":
		m.applyFilter("")
	case "1", "2", "3", "4", "5", "6":
		for _, p := range presets {
			if p.key == msg.String() {
				m.flat = true
				m.applyFilter(p.expr)
				m.setStatus("Preset: " + p.name + " (beneath the current folder)")
			}
		}
	case "s":
		m.sortBy = (m.sortBy + 1) % sortKey(len(sortNames))
		m.reverse = false
		m.rebuild()
	case "S":
		m.reverse = !m.reverse
		m.rebuild()
	case ".":
		m.showHidden = !m.showHidden
		m.rebuild()
	case " ", "space":
		if sel != nil {
			if m.marked[sel] {
				delete(m.marked, sel)
			} else {
				m.marked[sel] = true
			}
			m.cursor++
		}
	case "a":
		for _, r := range m.rows {
			m.marked[r] = true
		}
	case "u":
		clear(m.marked)
	case "d", "delete":
		m.confirmDelete(false)
	case "X":
		m.confirmDelete(true)
	case "o":
		if sel != nil {
			if err := reveal(sel.Path()); err != nil {
				m.setErr(err.Error())
			}
		}
	case "r":
		if m.busy == "" {
			return m, tea.Batch(m.startScan(m.cur.Path(), m.cur), tick())
		}
	case "c":
		m.culprits = findCulprits(m.root, m.home)
		m.culpritSel = 0
		m.mode = modeCulprits
	case "?":
		m.mode = modeHelp
	}
	m.clamp()
	return m, nil
}

func (m *model) updateFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if m.applyFilter(m.input.Value()) {
			m.mode = modeBrowse
			m.input.Blur()
			if m.filter.Active() && !m.flat {
				// filters are most useful across everything beneath; f goes back to tree view
				m.flat = true
				m.rebuild()
			}
		}
		return m, nil
	case "esc":
		m.mode = modeBrowse
		m.input.Blur()
		return m, nil
	}
	m.status = ""
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.mode = modeBrowse
	if msg.String() != "y" && msg.String() != "Y" {
		m.setStatus("Cancelled")
		return m, nil
	}
	targets, permanent := m.targets, m.permanent
	paths := make([]string, len(targets))
	for i, t := range targets {
		paths[i] = t.Path()
	}
	m.busy = "Moving to Trash"
	if permanent {
		m.busy = "Deleting"
	}
	return m, tea.Batch(tick(), func() tea.Msg {
		var err error
		if permanent {
			err = deletePermanently(paths)
		} else {
			err = moveToTrash(paths)
		}
		res := deleteDoneMsg{err: err, permanent: permanent}
		for i, p := range paths {
			if _, statErr := os.Lstat(p); os.IsNotExist(statErr) {
				res.removed = append(res.removed, targets[i])
				res.freed += targets[i].Size
			}
		}
		return res
	})
}

func (m *model) updateCulprits(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.culpritSel = max(0, m.culpritSel-1)
	case "down", "j":
		m.culpritSel = min(len(m.culprits)-1, m.culpritSel+1)
	case "enter", "right", "l":
		if len(m.culprits) > 0 {
			n := m.culprits[m.culpritSel].node
			m.mode = modeBrowse
			if n.IsDir {
				m.enter(n, false)
			} else {
				m.memo[n.Parent] = n
				m.enter(n.Parent, false)
			}
		}
	case "q":
		return m, tea.Quit
	default:
		m.mode = modeBrowse
	}
	return m, nil
}

func (m *model) enter(dir *Node, flat bool) {
	if m.cur != nil && m.cursor < len(m.rows) && !m.flat {
		m.memo[m.cur] = m.rows[m.cursor]
	}
	m.cur, m.flat = dir, flat
	m.cursor, m.offset = 0, 0
	m.rebuild()
	if want := m.memo[dir]; want != nil {
		if i := slices.Index(m.rows, want); i >= 0 {
			m.cursor = i
		}
	}
	m.clamp()
}

func (m *model) applyFilter(expr string) bool {
	f, err := ParseFilter(expr)
	if err != nil {
		m.setErr(err.Error())
		return false
	}
	m.filter = f
	m.cursor = 0
	m.rebuild()
	return true
}

func (m *model) confirmDelete(permanent bool) {
	if m.busy != "" {
		return
	}
	var targets []*Node
	if len(m.marked) > 0 {
		for n := range m.marked {
			skip := false
			for a := range m.marked {
				if isAncestor(a, n) {
					skip = true
					break
				}
			}
			if !skip && n.Parent != nil {
				targets = append(targets, n)
			}
		}
	} else if sel := m.selected(); sel != nil {
		targets = []*Node{sel}
	}
	if len(targets) == 0 {
		return
	}
	slices.SortFunc(targets, func(a, b *Node) int { return int64Cmp(b.Size, a.Size) })
	m.targets, m.permanent, m.mode = targets, permanent, modeConfirm
}

func (m *model) rebuild() {
	if m.cur == nil {
		return
	}
	var rows []*Node
	if m.flat {
		rows = collectFlat(m.cur, m.filter, m.showHidden)
	} else {
		for _, c := range m.cur.Children {
			if !m.showHidden && isHidden(c.Name) {
				continue
			}
			if (m.filter.Type == "file" && c.IsDir) || (m.filter.Type == "dir" && !c.IsDir) {
				continue
			}
			if m.filter.Match(c, c.Path) {
				rows = append(rows, c)
			}
		}
	}
	sortNodes(rows, m.sortBy, m.reverse)
	m.rows = rows
	m.clamp()
}

func sortNodes(rows []*Node, k sortKey, rev bool) {
	slices.SortStableFunc(rows, func(a, b *Node) int {
		var c int
		switch k {
		case sortSize:
			c = int64Cmp(b.Size, a.Size)
		case sortModified:
			c = a.Newest.Compare(b.Newest)
		case sortCreated:
			c = a.Created.Compare(b.Created)
		case sortAccessed:
			c = a.LastAccess.Compare(b.LastAccess)
		case sortItems:
			c = int64Cmp(b.Files, a.Files)
		}
		if c == 0 {
			c = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		if rev {
			c = -c
		}
		return c
	})
}

func (m *model) selected() *Node {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return nil
}

func (m *model) listHeight() int { return max(1, m.h-9) }

func (m *model) clamp() {
	m.cursor = max(0, min(m.cursor, len(m.rows)-1))
	lh := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+lh {
		m.offset = m.cursor - lh + 1
	}
	m.offset = max(0, min(m.offset, len(m.rows)-lh))
}

func (m *model) setStatus(s string) { m.status, m.statusErr = s, false }
func (m *model) setErr(s string)    { m.status, m.statusErr = s, true }

// ---------------------------------------------------------------- view

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#A89BFF"}
	cDim    = lipgloss.AdaptiveColor{Light: "#888888", Dark: "#777777"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#B26B00", Dark: "#E5B567"}
	cOld    = lipgloss.AdaptiveColor{Light: "#C2410C", Dark: "#F28B54"}
	cRed    = lipgloss.AdaptiveColor{Light: "#C62828", Dark: "#FF6B6B"}
	cGreen  = lipgloss.AdaptiveColor{Light: "#2E7D32", Dark: "#7BD88F"}
	cDir    = lipgloss.AdaptiveColor{Light: "#1E5AA8", Dark: "#7AB8FF"}

	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sBold   = lipgloss.NewStyle().Bold(true)
	sDir    = lipgloss.NewStyle().Foreground(cDir).Bold(true)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sOld    = lipgloss.NewStyle().Foreground(cOld)
	sRed    = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	sGreen  = lipgloss.NewStyle().Foreground(cGreen)
	sMark   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sBar    = lipgloss.NewStyle().Foreground(cAccent)
	sSel    = lipgloss.NewStyle().Reverse(true)
	sHeader = lipgloss.NewStyle().Foreground(cDim).Underline(true)
)

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	if m.root == nil {
		return m.viewScanning()
	}
	switch m.mode {
	case modeHelp:
		return m.viewHelp()
	case modeCulprits:
		return m.viewCulprits()
	}
	lines := m.viewHeader()
	lines = append(lines, m.viewList()...)
	lines = append(lines, m.viewDetail()...)
	lines = append(lines, m.viewFooter())
	return strings.Join(lines, "\n")
}

func (m *model) spinner() string {
	return string([]rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")[m.frame%10])
}

func (m *model) viewScanning() string {
	if m.status != "" {
		return sRed.Render(m.status) + "\n"
	}
	el := time.Since(m.scanFrom).Round(time.Second)
	return strings.Join([]string{
		"",
		"  " + sTitle.Render("freemymac") + "  " + m.spinner() + " scanning " + sBold.Render(m.tildify(m.rootPath)),
		"",
		fmt.Sprintf("  %s files · %s · %s · %d unreadable",
			commas(m.prog.Files.Load()), human(m.prog.Bytes.Load()), el, m.prog.Errors.Load()),
		"  " + sDim.Render(truncLeft(m.tildify(m.prog.Current()), m.w-4)),
		"",
		sDim.Render("  Tip: grant your terminal Full Disk Access (System Settings › Privacy & Security)"),
		sDim.Render("  to include protected folders like Mail and Messages. q to quit."),
	}, "\n")
}

func (m *model) viewHeader() []string {
	disk := ""
	if m.diskTotal > 0 {
		disk = fmt.Sprintf("disk: %s free of %s", human(int64(m.diskFree)), human(int64(m.diskTotal)))
	}
	left := sTitle.Render("freemymac") + "  " + sBold.Render(truncLeft(m.tildify(m.cur.Path()), m.w/2)) +
		"  " + human(m.cur.Size) + sDim.Render(fmt.Sprintf(" · %s files", commas(m.cur.Files)))
	l1 := spread(left, sDim.Render(disk), m.w)

	view := "tree"
	if m.flat {
		view = "flat (all files beneath)"
		if m.filter.Type == "dir" {
			view = "flat (folders beneath)"
		}
	}
	dir := "↑"
	if (m.sortBy == sortSize || m.sortBy == sortItems) != m.reverse {
		dir = "↓"
	}
	parts := []string{
		"view " + sBold.Render(view),
		"sort " + sBold.Render(sortNames[m.sortBy]+dir),
	}
	if !m.showHidden {
		parts = append(parts, sWarn.Render("hidden files off"))
	}
	if m.filter.Active() {
		parts = append(parts, "filter "+sWarn.Render(m.filter.Raw))
	}
	var total int64
	for _, r := range m.rows {
		total += r.Size
	}
	parts = append(parts, fmt.Sprintf("%s shown · %s", commas(int64(len(m.rows))), human(total)))
	l2 := sDim.Render(strings.Join(parts, sDim.Render("  │  ")))
	if m.mode == modeFilter {
		l2 = m.input.View()
	}
	return []string{l1, l2}
}

type layout struct {
	bar, items, created, accessed bool
	nameW                         int
}

func (m *model) layout() layout {
	l := layout{bar: m.w >= 70, items: m.w >= 80, created: m.w >= 95, accessed: m.w >= 125}
	w := 2 + 9 + 1 + 12 // mark, size, modified
	if l.bar {
		w += 12
	}
	if l.items {
		w += 8
	}
	if l.created {
		w += 12
	}
	if l.accessed {
		w += 12
	}
	l.nameW = max(10, m.w-w-1)
	return l
}

func (m *model) viewList() []string {
	l := m.layout()
	lh := m.listHeight()
	arrow := func(k sortKey, label string, w int, right bool) string {
		if m.sortBy == k {
			label += map[bool]string{true: "▲", false: "▼"}[(k == sortSize || k == sortItems) == m.reverse]
		}
		if right {
			return fmt.Sprintf("%*s", w, label)
		}
		return fmt.Sprintf("%-*s", w, label)
	}
	h := "  " + arrow(sortSize, "Size", 9, true) + " "
	if l.bar {
		h += strings.Repeat(" ", 12)
	}
	if l.items {
		h += arrow(sortItems, "Files", 7, true) + " "
	}
	if l.created {
		h += arrow(sortCreated, "Created", 10, false) + "  "
	}
	h += arrow(sortModified, "Modified", 10, false) + "  "
	if l.accessed {
		h += arrow(sortAccessed, "Accessed", 10, false) + "  "
	}
	h += arrow(sortName, "Name", 4, false)
	out := []string{sHeader.Render(runewidth.FillRight(h, m.w))}

	ref := m.cur.Size
	if m.flat && len(m.rows) > 0 {
		ref = 0
		for _, r := range m.rows {
			ref = max(ref, r.Size)
		}
	}
	base := m.cur.Path()
	for i := m.offset; i < m.offset+lh; i++ {
		if i >= len(m.rows) {
			if i == 0 {
				msg := "  (empty)"
				if m.filter.Active() {
					msg = "  nothing matches the filter (esc clears it)"
				}
				out = append(out, sDim.Render(msg))
				continue
			}
			out = append(out, "")
			continue
		}
		out = append(out, m.viewRow(m.rows[i], i == m.cursor, l, ref, base))
	}
	return out
}

func (m *model) viewRow(n *Node, selected bool, l layout, ref int64, base string) string {
	plain := selected
	st := func(s lipgloss.Style, v string) string {
		if plain {
			return v
		}
		return s.Render(v)
	}
	var b strings.Builder
	if m.marked[n] {
		b.WriteString(st(sMark, "● "))
	} else {
		b.WriteString("  ")
	}
	b.WriteString(fmt.Sprintf("%9s ", human(n.Size)))
	if l.bar {
		frac := 0.0
		if ref > 0 {
			frac = float64(n.Size) / float64(ref)
		}
		b.WriteString(st(sBar, bar(frac, 10)) + "  ")
	}
	if l.items {
		items := ""
		if n.IsDir {
			items = compact(n.Files)
		}
		b.WriteString(st(sDim, fmt.Sprintf("%7s", items)) + " ")
	}
	if l.created {
		b.WriteString(st(sDim, fmt.Sprintf("%-10s", date(n.Created))) + "  ")
	}
	b.WriteString(st(ageStyle(n.Newest), fmt.Sprintf("%-10s", date(n.Newest))) + "  ")
	if l.accessed {
		b.WriteString(st(ageStyle(n.LastAccess), fmt.Sprintf("%-10s", date(n.LastAccess))) + "  ")
	}

	name := n.Name
	if m.flat {
		if rel, err := filepath.Rel(base, n.Path()); err == nil {
			name = rel
		}
	}
	suffix := ""
	switch {
	case n.IsDir:
		suffix = "/"
	case n.IsLink:
		suffix = "@"
	}
	if n.Err != "" {
		suffix += " ⚠"
	}
	label := truncLeft(name+suffix, l.nameW)
	if n.IsDir {
		b.WriteString(st(sDir, label))
	} else {
		b.WriteString(label)
	}
	line := b.String()
	if selected {
		return sSel.Render(runewidth.FillRight(line, m.w))
	}
	return line
}

func (m *model) viewDetail() []string {
	sep := sDim.Render(strings.Repeat("─", m.w))
	n := m.selected()
	if n == nil {
		return []string{sep, "", "", "", ""}
	}
	kind := "file"
	if n.IsDir {
		kind = "folder"
	} else if n.IsLink {
		kind = "symlink"
	}
	pct := 0.0
	if m.cur.Size > 0 {
		pct = 100 * float64(n.Size) / float64(m.cur.Size)
	}
	sizeLine := fmt.Sprintf("%s %s on disk (logical %s) · %.1f%% of current folder",
		kind, sBold.Render(human(n.Size)), human(n.Apparent), pct)
	if n.IsDir {
		sizeLine += fmt.Sprintf(" · %s files", commas(n.Files))
	}
	times := fmt.Sprintf("created %s   modified %s", stamp(n.Created), stamp(n.Modified))
	extra := fmt.Sprintf("last opened %s", stamp(n.Accessed))
	if n.IsDir {
		times = fmt.Sprintf("created %s   folder modified %s", stamp(n.Created), stamp(n.Modified))
		extra = fmt.Sprintf("newest change inside %s   last file opened inside %s", stamp(n.Newest), stamp(n.LastAccess))
	}
	note := ""
	if hint, ok := m.hints[n.Path()]; ok {
		note = sGreen.Render("💡 " + hint)
	} else if hint, ok := culpritNames[n.Name]; ok && n.IsDir {
		note = sGreen.Render("💡 " + hint)
	}
	if n.HardLinked {
		note = sDim.Render(fmt.Sprintf("hard link: same data as other names, counted once (logical %s)", human(n.Apparent)))
	}
	if n.Err != "" {
		note = sWarn.Render("⚠ " + n.Err)
	}
	return []string{
		sep,
		sBold.Render(truncLeft(m.tildify(n.Path()), m.w)),
		sizeLine,
		sDim.Render(times),
		sDim.Render(truncLeft(extra, m.w)) + "  " + note,
	}
}

func (m *model) viewFooter() string {
	right := ""
	if len(m.marked) > 0 {
		var sum int64
		for n := range m.marked {
			sum += n.Size
		}
		right = sMark.Render(fmt.Sprintf("● %d marked · %s", len(m.marked), human(sum)))
	}
	switch {
	case m.mode == modeConfirm:
		return m.viewConfirm()
	case m.scanning:
		return spread(fmt.Sprintf("%s rescanning… %s files · %s", m.spinner(),
			commas(m.prog.Files.Load()), human(m.prog.Bytes.Load())), right, m.w)
	case m.busy != "":
		return spread(m.spinner()+" "+m.busy+"…", right, m.w)
	case m.mode == modeFilter && m.statusErr:
		return sRed.Render(m.status)
	case m.mode == modeFilter:
		return sDim.Render("enter apply · esc cancel · keys: name: ext: path: type: size> mod> created> accessed>  (! negates)")
	case m.status != "":
		s := sGreen
		if m.statusErr {
			s = sRed
		}
		return spread(s.Render(runewidth.Truncate(m.status, m.w-lipgloss.Width(right)-2, "…")), right, m.w)
	}
	help := "↑↓ move  ⏎ open  ← back  f flat  / filter  1-6 presets  s sort  ␣ mark  d trash  c culprits  o finder  ? help  q quit"
	return spread(sDim.Render(runewidth.Truncate(help, m.w-lipgloss.Width(right)-2, "…")), right, m.w)
}

func (m *model) viewConfirm() string {
	var sum int64
	for _, t := range m.targets {
		sum += t.Size
	}
	what := fmt.Sprintf("%d items", len(m.targets))
	if len(m.targets) == 1 {
		what = "“" + m.targets[0].Name + "”"
	}
	if m.permanent {
		return sRed.Render(fmt.Sprintf("PERMANENTLY delete %s (%s)? This cannot be undone. [y/N]", what, human(sum)))
	}
	return sWarn.Render(fmt.Sprintf("Move %s (%s) to the Trash? [y/N]", what, human(sum)))
}

func (m *model) viewCulprits() string {
	lines := []string{
		sTitle.Render("Common space hogs") + sDim.Render("  (found in the scanned tree · ⏎ jump · esc back)"),
		"",
	}
	if len(m.culprits) == 0 {
		lines = append(lines, sDim.Render("  None of the usual suspects were found under "+m.tildify(m.rootPath)))
	}
	vis := max(1, m.h-3)
	start := max(0, m.culpritSel-vis+1)
	pathW := 0
	for _, c := range m.culprits {
		pathW = max(pathW, runewidth.StringWidth(m.tildify(c.node.Path())))
	}
	pathW = min(pathW, m.w/2)
	for i := start; i < len(m.culprits) && i < start+vis; i++ {
		c := m.culprits[i]
		line := fmt.Sprintf("  %9s  %s  ", human(c.node.Size),
			runewidth.FillRight(truncLeft(m.tildify(c.node.Path()), pathW), pathW))
		desc := runewidth.Truncate(c.desc, max(0, m.w-runewidth.StringWidth(line)), "…")
		if i == m.culpritSel {
			lines = append(lines, sSel.Render(runewidth.FillRight(line+desc, m.w)))
		} else {
			lines = append(lines, line+sDim.Render(desc))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewHelp() string {
	var p []string
	for _, pr := range presets {
		p = append(p, fmt.Sprintf("    %s  %-24s %s", pr.key, pr.name, sDim.Render(pr.expr)))
	}
	return strings.Join(append([]string{
		sTitle.Render("freemymac") + " — find what is eating your disk",
		"",
		sBold.Render("Navigate") + "   ↑/↓ j/k  pgup/pgdn  g/G top/bottom   ⏎/→ open folder   ←/⌫ parent",
		"           in flat view, ⏎ on a file jumps to its folder",
		sBold.Render("Views") + "      f  toggle flat view (every file beneath this folder, biggest first)",
		"           c  common space hogs (caches, Xcode, Docker, backups…)",
		"           s  cycle sort (size/modified/created/accessed/name/files)   S  reverse",
		"           .  show/hide dotfiles   r  rescan this folder   o  reveal in Finder",
		sBold.Render("Clean up") + "   space mark   a mark all shown   u unmark all",
		"           d  move marked (or selected) to Trash      X  delete permanently",
		"",
		sBold.Render("Filter") + " (/)  space-separated conditions, all must match; prefix ! to negate",
		"    report          name contains “report”      name:*.log,*.tmp   globs, comma = or",
		"    ext:dmg,zip     by extension                path:Library       full path contains",
		"    size>500M       on-disk size (K M G T)      type:dir|file|any  what to list",
		"    mod>1y          not modified in a year      mod<7d             modified this week",
		"    created>2y      created over 2 years ago    accessed>6m        not opened in 6 months",
		"    ages: h d w m(onths) y.  Folders use the newest date of anything inside them.",
		"    In flat view, type:dir lists matching folders without descending into them.",
		"",
		sBold.Render("Presets") + " (switch to flat view; 0 or esc clears)",
	}, p...), "\n") + "\n\n" + sDim.Render("    Sizes are real disk usage (APFS clones/compression counted as stored), base-10 like Finder.") +
		"\n" + sDim.Render("    press any key")
}

// ---------------------------------------------------------------- helpers

func (m *model) tildify(p string) string {
	if m.home != "" && (p == m.home || strings.HasPrefix(p, m.home+"/")) {
		return "~" + p[len(m.home):]
	}
	return p
}

func human(b int64) string {
	const u = 1000
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	v, i := float64(b), 0
	for v >= u && i < 5 {
		v /= u
		i++
	}
	unit := []string{"", "kB", "MB", "GB", "TB", "PB"}[i]
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, unit)
	}
	return fmt.Sprintf("%.0f %s", v, unit)
}

func compact(n int64) string {
	switch {
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func date(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "-"
	}
	return t.Format("2006-01-02")
}

func stamp(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "-"
	}
	return t.Format("2006-01-02 15:04") + " (" + ago(t) + ")"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dmin ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%.1fy ago", d.Hours()/24/365)
}

func ageStyle(t time.Time) lipgloss.Style {
	switch d := time.Since(t); {
	case t.IsZero():
		return sDim
	case d > 2*365*24*time.Hour:
		return sOld
	case d > 365*24*time.Hour:
		return sWarn
	}
	return lipgloss.NewStyle()
}

func bar(frac float64, w int) string {
	frac = max(0, min(1, frac))
	eighths := int(frac*float64(w*8) + 0.5)
	full, rem := eighths/8, eighths%8
	s := strings.Repeat("█", full)
	if rem > 0 {
		s += string([]rune(" ▏▎▍▌▋▊▉")[rem])
	}
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

func truncLeft(s string, w int) string {
	if w <= 1 || runewidth.StringWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for i := range r {
		if runewidth.StringWidth(string(r[i:])) <= w-1 {
			return "…" + string(r[i:])
		}
	}
	return "…"
}

func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}
