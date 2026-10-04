// metaflac: edit FLAC Vorbis comments in a terminal. Usage: metaflac song.flac
package main

import (
	"fmt"
	_ "image/jpeg" // decoders for flacpicture
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/go-flac/flacpicture"
	"github.com/go-flac/flacvorbis"
	"github.com/go-flac/go-flac"
)

type model struct {
	path    string
	file    *flac.File
	vorbis  *flacvorbis.MetaDataBlockVorbisComment
	idx     int    // index of vorbis block in file.Meta, -1 if none
	cur     int    // selected line
	editing bool   // line editor active
	picking bool   // editor holds a cover image path, not a tag
	buf     string // editor contents ("KEY=VALUE")
	status  string
	dirty   bool // unsaved tag edits
	dir     string
	ents    []string // "../", "sub/", "x.flac"
	ecur    int
	side    bool // explorer has focus

	w, h int // terminal size

	pvPath, pvCover string   // file under the explorer cursor, shown read-only
	pvLines         []string // its tags; "" pvPath means no preview

	marked     map[string]bool // abs paths; non-empty means batch mode
	batch      []string        // batch mode: "KEY=VALUE" to set, "KEY=" to delete
	batchCover string          // batch mode: cover image path to embed
}

var (
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	blue   = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
)

func abs(p string) string {
	a, _ := filepath.Abs(p)
	return a
}

func (m model) batchMode() bool { return len(m.marked) > 0 }

func (m model) markedPaths() []string {
	ps := make([]string, 0, len(m.marked))
	for p := range m.marked {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// row is one selectable explorer line.
type row struct {
	label, path string
	dir, marked bool
}

func (m model) rows() []row {
	var rs []row
	for _, n := range m.ents {
		p := filepath.Join(m.dir, n)
		rs = append(rs, row{label: n, path: p, dir: strings.HasSuffix(n, "/"), marked: m.marked[abs(p)]})
	}
	return rs
}

// lines is the tag list being edited: the batch list or the open file's tags.
func (m *model) lines() *[]string {
	if m.batchMode() {
		return &m.batch
	}
	return &m.vorbis.Comments
}

func (m model) canEdit() bool { return m.batchMode() || m.path != "" }

// touch flags the open file as changed; batch edits have nothing to lose.
func (m *model) touch() {
	if !m.batchMode() {
		m.dirty = true
	}
}

func (m *model) readDir() {
	m.ents, m.ecur = []string{"../"}, 0
	es, _ := os.ReadDir(m.dir) // An unreadable dir just shows "../".
	for _, e := range es {
		n := e.Name()
		switch {
		case strings.HasPrefix(n, "."):
		case e.IsDir():
			m.ents = append(m.ents, n+"/")
		case strings.EqualFold(filepath.Ext(n), ".flac"):
			m.ents = append(m.ents, n)
		}
	}
}

func (m *model) open(path string) error {
	f, err := flac.ParseFile(path)
	if err != nil {
		return err
	}
	v, idx := flacvorbis.New(), -1
	for i, b := range f.Meta {
		if b.Type == flac.VorbisComment {
			if v, err = flacvorbis.ParseFromMetaDataBlock(*b); err != nil {
				return err
			}
			idx = i
		}
	}
	m.path, m.file, m.vorbis, m.idx, m.cur, m.dirty = path, f, v, idx, 0, false
	return nil
}

func load(path string) (*model, error) {
	m := &model{}
	return m, m.open(path)
}

func (m *model) save() error {
	b := m.vorbis.Marshal()
	if m.idx < 0 { // No comment block yet: append one.
		m.file.Meta = append(m.file.Meta, &b)
		m.idx = len(m.file.Meta) - 1
	} else {
		m.file.Meta[m.idx] = &b
	}
	if err := m.file.Save(m.path); err != nil {
		return err
	}
	m.dirty, m.pvPath = false, ""
	return nil
}

// loadCover builds a front-cover picture block from a jpeg/png file.
func loadCover(path string) (flac.MetaDataBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return flac.MetaDataBlock{}, err
	}
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" {
		return flac.MetaDataBlock{}, fmt.Errorf("not a jpeg/png: %s", mime)
	}
	p, err := flacpicture.NewFromImageData(flacpicture.PictureTypeFrontCover, "", data, mime)
	if err != nil {
		return flac.MetaDataBlock{}, err
	}
	return p.Marshal(), nil
}

// setCover replaces the front cover (or adds one) from an image file.
func (m *model) setCover(path string) error {
	b, err := loadCover(path)
	if err != nil {
		return err
	}
	for i, o := range m.file.Meta {
		if q, err := flacpicture.ParseFromMetaDataBlock(*o); o.Type == flac.Picture && err == nil && q.PictureType == flacpicture.PictureTypeFrontCover {
			m.file.Meta[i] = &b
			return nil
		}
	}
	m.file.Meta = append(m.file.Meta, &b)
	return nil
}

// applyBatch sets/deletes the batch tags (and cover) on every marked file, keeping their other tags.
func (m *model) applyBatch() string {
	keys := map[string]bool{}
	for _, l := range m.batch {
		k, _, _ := strings.Cut(l, "=")
		keys[strings.ToUpper(k)] = true
	}
	ps, fails := m.markedPaths(), []string(nil)
	for i, p := range ps {
		// {n}, {nn} and {total} become the file's running number (by sorted path) and the file count.
		num := strings.NewReplacer("{nn}", fmt.Sprintf("%02d", i+1), "{n}", fmt.Sprint(i+1), "{total}", fmt.Sprint(len(ps)))
		var add []string
		for _, l := range m.batch {
			if _, v, _ := strings.Cut(l, "="); v != "" {
				add = append(add, num.Replace(l))
			}
		}
		t, err := load(p)
		if err == nil {
			keep := t.vorbis.Comments[:0]
			for _, l := range t.vorbis.Comments {
				if k, _, _ := strings.Cut(l, "="); !keys[strings.ToUpper(k)] {
					keep = append(keep, l)
				}
			}
			t.vorbis.Comments = append(keep, add...)
			if m.batchCover != "" {
				err = t.setCover(m.batchCover)
			}
		}
		if err == nil {
			err = t.save()
		}
		if err != nil {
			fails = append(fails, filepath.Base(p)+": "+err.Error())
		}
	}
	m.pvPath = ""
	if m.path != "" && m.marked[abs(m.path)] {
		m.open(m.path) // show the new tags; batch mode forbids unsaved edits, so nothing is lost
	}
	if len(fails) > 0 {
		return fmt.Sprintf("applied %d/%d, failed: %s", len(ps)-len(fails), len(ps), strings.Join(fails, "; "))
	}
	m.batch, m.batchCover = nil, ""
	return fmt.Sprintf("applied to %d files", len(ps))
}

func (m model) cover() string {
	for _, b := range m.file.Meta {
		if p, err := flacpicture.ParseFromMetaDataBlock(*b); b.Type == flac.Picture && err == nil && p.PictureType == flacpicture.PictureTypeFrontCover {
			return fmt.Sprintf("%s %dx%d, %d KB", p.MIME, p.Width, p.Height, len(p.ImageData)/1024)
		}
	}
	return "none"
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	n, c := m.update(msg)
	nm := n.(model)
	if nm.side {
		nm.refreshPreview()
	}
	return nm, c
}

// refreshPreview loads the tags of the .flac under the explorer cursor.
func (m *model) refreshPreview() {
	rs := m.rows()
	if m.ecur >= len(rs) || rs[m.ecur].dir {
		m.pvPath = ""
		return
	}
	r := rs[m.ecur]
	if p := abs(r.path); p != m.pvPath {
		m.pvPath, m.pvCover, m.pvLines = p, "", nil
		if t, err := load(r.path); err != nil {
			m.pvLines = []string{err.Error()}
		} else {
			m.pvCover, m.pvLines = t.cover(), t.vorbis.Comments
		}
	}
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if w, ok := msg.(tea.WindowSizeMsg); ok {
		m.w, m.h = w.Width, w.Height
		return m, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if m.side {
		return m.updateSide(k)
	}
	c := m.lines()
	if m.editing {
		switch k.Type {
		case tea.KeyEnter:
			if m.picking {
				if m.batchMode() {
					_, err := loadCover(m.buf)
					if err != nil {
						m.status = err.Error()
						return m, nil
					}
					m.batchCover = m.buf
				} else if err := m.setCover(m.buf); err != nil {
					m.status = err.Error()
					return m, nil
				}
				m.editing, m.picking, m.status = false, false, "cover set, press s to save"
				m.touch()
				return m, nil
			}
			if strings.IndexByte(m.buf, '=') < 1 {
				m.status = "need KEY=VALUE"
				return m, nil
			}
			(*c)[m.cur] = m.buf
			m.editing = false
			m.touch()
		case tea.KeyEsc:
			m.editing = false
			if m.picking {
				m.picking = false
			} else if (*c)[m.cur] == "" { // cancelled a fresh line
				*c = append((*c)[:m.cur], (*c)[m.cur+1:]...)
				m.cur = max(0, m.cur-1)
			}
		case tea.KeyBackspace:
			if r := []rune(m.buf); len(r) > 0 {
				m.buf = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			m.buf += string(k.Runes)
		}
		return m, nil
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.cur = max(0, m.cur-1)
	case "down", "j":
		m.cur = min(len(*c)-1, m.cur+1)
	case "enter", "e":
		if len(*c) > 0 {
			m.editing, m.buf, m.status = true, (*c)[m.cur], ""
		}
	case "c":
		if m.canEdit() {
			m.editing, m.picking, m.buf, m.status = true, true, "", ""
		}
	case "a":
		if m.canEdit() {
			*c = append(*c, "")
			m.cur, m.editing, m.buf, m.status = len(*c)-1, true, "", ""
		}
	case "d":
		if len(*c) > 0 {
			*c = append((*c)[:m.cur], (*c)[m.cur+1:]...)
			m.cur = max(0, min(m.cur, len(*c)-1))
			m.touch()
		}
	case "tab":
		m.side = true
	case "r":
		if m.batchMode() {
			clear(m.marked)
			m.batch, m.batchCover, m.cur, m.status, m.side = nil, "", 0, "batch cleared", true
		} else if m.path != "" {
			if err := m.open(m.path); err != nil {
				m.status = err.Error()
			} else {
				m.status = "reverted"
			}
		}
	case "s":
		if m.batchMode() {
			if len(m.batch) == 0 && m.batchCover == "" {
				m.status = "nothing to apply"
			} else {
				m.status = m.applyBatch()
			}
			m.cur = min(m.cur, max(0, len(m.batch)-1))
			break
		}
		if m.path == "" {
			break
		}
		if err := m.save(); err != nil {
			m.status = "save failed: " + err.Error()
		} else {
			m.status = "saved"
		}
	}
	return m, nil
}

func (m model) updateSide(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	rs := m.rows()
	r := rs[m.ecur]
	switch {
	case k.Type == tea.KeySpace:
		if r.dir {
			break
		}
		a := abs(r.path)
		if m.marked[a] {
			delete(m.marked, a)
		} else if m.dirty {
			m.status = "unsaved: save or revert before marking"
			break
		} else {
			m.marked[a] = true
		}
		if !m.batchMode() {
			m.batch, m.batchCover, m.cur = nil, "", 0
		}
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.side = !m.canEdit()
	case "up", "k":
		m.ecur = max(0, m.ecur-1)
	case "down", "j":
		m.ecur = min(len(rs)-1, m.ecur+1)
	case "enter", "l":
		switch {
		case r.dir:
			m.dir = r.path
			m.readDir()
		case m.batchMode():
			m.status, m.side = "", false
		case m.dirty:
			m.status = "unsaved: tab back, then s save or r revert"
		default:
			if err := m.open(r.path); err != nil {
				m.status = err.Error()
			} else {
				m.status, m.side = "", false
			}
		}
	}
	return m, nil
}

// window returns the h lines around cur so the cursor stays on screen.
func window(ls []string, cur, h int) []string {
	h = max(h, 1)
	if len(ls) <= h {
		return ls
	}
	start := max(0, min(cur-h/2, len(ls)-h))
	return ls[start : start+h]
}

// fit makes a line single-row and at most w cells wide; the editing line keeps its tail (the cursor).
func fit(l string, w int, tail bool) string {
	l = strings.ReplaceAll(l, "\n", "⏎")
	if tail && ansi.StringWidth(l) > w {
		return "…" + ansi.TruncateLeft(l, ansi.StringWidth(l)-w+1, "")
	}
	return ansi.Truncate(l, w, "…")
}

func (m model) View() string {
	w, h := m.w, m.h
	if w == 0 {
		w, h = 80, 24
	}
	sideW, tagW := 30, max(w-37, 10)
	rs, np := m.rows(), len(m.marked)
	sl := []string{fmt.Sprintf("selected: %d", np), "", fit(m.dir, sideW, false), ""}
	scur := 0
	for i, r := range rs {
		p, mk := "  ", "  "
		if i == m.ecur {
			p, scur = "> ", len(sl)
		}
		if r.marked {
			mk = "* "
		}
		line := fit(p+mk+r.label, sideW, false)
		if m.side && i == m.ecur && m.pvPath != "" {
			line = accent.Render(line)
		} else if r.marked {
			line = yellow.Render(line)
		}
		sl = append(sl, line)
	}
	var tl []string
	var lines []string
	preview := m.side && m.pvPath != ""
	switch {
	case preview:
		tl = []string{filepath.Base(m.pvPath) + " (preview)", "cover: " + m.pvCover, ""}
		lines = m.pvLines
	case m.batchMode():
		cv := m.batchCover
		if cv == "" {
			cv = "unchanged"
		}
		tl = []string{fmt.Sprintf("BATCH: %d marked files", np), "cover: " + cv, ""}
		lines = m.batch
	case m.path != "":
		tl = []string{filepath.Base(m.path), "cover: " + m.cover(), ""}
		lines = m.vorbis.Comments
	default:
		tl = []string{"pick a .flac file"}
	}
	for i, l := range tl {
		tl[i] = fit(l, tagW, false)
	}
	disp := make([]string, len(lines))
	for i, l := range lines {
		p, edit := "  ", m.editing && !m.picking && i == m.cur
		if i == m.cur && !preview {
			p = "> "
		}
		if edit {
			l = m.buf + "█"
		}
		disp[i] = fit(p+l, tagW, edit)
	}
	cur := m.cur
	if preview {
		cur = 0
	}
	tl = append(tl, window(disp, cur, h-4-len(tl))...)

	help := "tab files  ↑/↓ move  enter edit  a add  d delete  c cover  s save  r revert  q quit"
	switch {
	case m.side:
		help = "tab tags  ↑/↓ move  space mark  enter edit  q quit"
	case m.editing && m.picking:
		help = "type jpeg/png path  enter accept  esc cancel"
	case m.editing && m.batchMode():
		help = "KEY=VALUE, KEY= deletes, {n} {nn} {total} number the files  enter accept  esc cancel"
	case m.editing:
		help = "type KEY=VALUE  enter accept  esc cancel"
	case m.batchMode():
		help = "tab files  ↑/↓ move  enter edit  a add  d delete  c cover  s apply to marked  r clear marks  q quit"
	}
	status, border := m.status, lipgloss.Color("8")
	if m.picking {
		status = "cover path: " + m.buf + "█"
	}
	if preview {
		border = accent.GetForeground().(lipgloss.Color)
	}
	if m.editing {
		status = blue.Render("EDITING") + " " + status
		border = blue.GetForeground().(lipgloss.Color)
	}
	box := lipgloss.NewStyle().Padding(0, 1)
	left := box.Width(32).BorderStyle(lipgloss.NormalBorder()).BorderRight(true).Render(strings.Join(window(sl, scur, h-2), "\n"))
	right := box.BorderStyle(lipgloss.NormalBorder()).BorderForeground(border).Render(strings.Join(tl, "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	return body + "\n" + fit(status, w, false) + "\n" + fit(help, w, false)
}

func main() {
	arg := "."
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: metaflac [file.flac|dir]")
		os.Exit(2)
	} else if len(os.Args) == 2 {
		arg = os.Args[1]
	}
	m := &model{side: true, marked: map[string]bool{}}
	st, err := os.Stat(arg)
	if err == nil && !st.IsDir() {
		m.dir, m.side = filepath.Dir(arg), false
		err = m.open(arg)
	} else {
		m.dir = arg
	}
	if err == nil {
		m.readDir()
		_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
