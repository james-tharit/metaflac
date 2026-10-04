package main

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.flac")
	if err := exec.Command("ffmpeg", "-f", "lavfi", "-i", "sine=d=1", p).Run(); err != nil {
		t.Skip("no ffmpeg")
	}
	m, err := load(p)
	if err != nil {
		t.Fatal(err)
	}
	m.vorbis.Add("TITLE", "hi")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2, err := load(p)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m2.vorbis.Get("TITLE"); len(v) != 1 || v[0] != "hi" {
		t.Fatalf("got %v", v)
	}
	if err := exec.Command("ffmpeg", "-v", "error", "-i", p, "-f", "null", "-").Run(); err != nil {
		t.Fatal("audio corrupted:", err)
	}
}

func TestCover(t *testing.T) {
	d := t.TempDir()
	p, img := filepath.Join(d, "a.flac"), filepath.Join(d, "c.png")
	if exec.Command("ffmpeg", "-f", "lavfi", "-i", "sine=d=1", p).Run() != nil ||
		exec.Command("ffmpeg", "-f", "lavfi", "-i", "color=red:s=8x8", "-frames:v", "1", img).Run() != nil {
		t.Skip("no ffmpeg")
	}
	m, _ := load(p)
	for range 2 { // second call must replace, not duplicate
		if err := m.setCover(img); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m, _ = load(p)
	if c := m.cover(); !strings.HasPrefix(c, "image/png 8x8") {
		t.Fatal(c)
	}
	if err := m.setCover(p); err == nil {
		t.Fatal("non-image accepted")
	}
}

func TestBatch(t *testing.T) {
	d := t.TempDir()
	var ps []string
	for _, n := range []string{"a", "b"} {
		p := filepath.Join(d, n+".flac")
		if exec.Command("ffmpeg", "-f", "lavfi", "-i", "sine=d=1", p).Run() != nil {
			t.Skip("no ffmpeg")
		}
		m, _ := load(p)
		m.vorbis.Comments = []string{"TITLE=" + n, "ALBUM=old", "ARTIST=keep"}
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	m := &model{marked: map[string]bool{ps[0]: true, ps[1]: true}, batch: []string{"album=new", "TITLE="}}
	if s := m.applyBatch(); s != "applied to 2 files" {
		t.Fatal(s)
	}
	for _, p := range ps {
		f, _ := load(p)
		got := strings.Join(f.vorbis.Comments, ",")
		if !strings.Contains(got, "album=new") || !strings.Contains(got, "ARTIST=keep") ||
			strings.Contains(got, "TITLE") || strings.Contains(got, "ALBUM=old") {
			t.Fatal(got)
		}
	}
	m.batch = []string{"TRACKNUMBER={nn}", "TITLE=T{n}/{total}"}
	if s := m.applyBatch(); s != "applied to 2 files" {
		t.Fatal(s)
	}
	for i, p := range ps {
		f, _ := load(p)
		got := strings.Join(f.vorbis.Comments, ",")
		if want := fmt.Sprintf("TRACKNUMBER=0%d", i+1); !strings.Contains(got, want) || !strings.Contains(got, fmt.Sprintf("TITLE=T%d/2", i+1)) {
			t.Fatal(got)
		}
	}
}

func TestMark(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"a.flac", "b.flac"} {
		os.WriteFile(filepath.Join(d, n), nil, 0o644)
	}
	m := model{dir: d, marked: map[string]bool{}, w: 80, h: 20}
	m.readDir() // ../ a.flac b.flac
	m.ecur = 2  // b.flac
	space := tea.KeyMsg{Type: tea.KeySpace}
	n, _ := m.updateSide(space)
	m = n.(model)
	if !m.batchMode() || !m.rows()[2].marked || m.ecur != 2 || len(m.rows()) != 3 {
		t.Fatalf("%+v ecur=%d", m.rows(), m.ecur)
	}
	if v := m.View(); !strings.Contains(v, "selected: 1") || strings.Count(v, "b.flac") != 1 {
		t.Fatal(v)
	}
	n, _ = m.updateSide(space) // unmark
	if n.(model).batchMode() {
		t.Fatal("unmark failed")
	}
}

func TestViewFitsScreen(t *testing.T) {
	d := t.TempDir()
	for i := range 100 { // far more files than rows
		os.WriteFile(filepath.Join(d, fmt.Sprintf("%03d.flac", i)), nil, 0o644)
	}
	m := model{dir: d, marked: map[string]bool{}, w: 80, h: 20}
	m.readDir()
	m.ecur = 90
	if n := strings.Count(m.View(), "\n") + 1; n > 20 {
		t.Fatalf("view is %d lines on a 20-line screen", n)
	}
	if !strings.Contains(m.View(), "> ") || !strings.Contains(m.View(), "089.flac") {
		t.Fatal("cursor row scrolled off")
	}
	// editing a new line in a file with many tags keeps the editor visible
	m = model{marked: map[string]bool{"/x": true}, w: 80, h: 20, editing: true, buf: "NEW=v"}
	for i := range 60 {
		m.batch = append(m.batch, fmt.Sprintf("K%d=v", i))
	}
	m.batch, m.cur = append(m.batch, ""), 60
	if v := m.View(); !strings.Contains(v, "NEW=v█") || strings.Count(v, "\n")+1 > 20 {
		t.Fatal(v)
	}
}

func TestClearUnmarks(t *testing.T) {
	m := model{marked: map[string]bool{"/x": true}, batch: []string{"A=b"}}
	n, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if n.(model).batchMode() {
		t.Fatal("marks survived clear")
	}
}

func TestPreviewFollowsCursor(t *testing.T) {
	d := t.TempDir()
	var ps []string
	for _, n := range []string{"a", "b"} {
		p := filepath.Join(d, n+".flac")
		if exec.Command("ffmpeg", "-f", "lavfi", "-i", "sine=d=1", "-metadata", "TITLE="+n+"-title", p).Run() != nil {
			t.Skip("no ffmpeg")
		}
		ps = append(ps, p)
	}
	m := model{dir: d, side: true, marked: map[string]bool{}, w: 80, h: 20}
	m.readDir() // ../ a b
	var n tea.Model = m
	n, _ = n.Update(tea.KeyMsg{Type: tea.KeyDown})
	if v := n.View(); !strings.Contains(v, "a-title") || !strings.Contains(v, "(preview)") {
		t.Fatal(v)
	}
	n, _ = n.Update(tea.KeyMsg{Type: tea.KeyDown})
	if v := n.View(); !strings.Contains(v, "b-title") || strings.Contains(v, "a-title") {
		t.Fatal(v)
	}
}
