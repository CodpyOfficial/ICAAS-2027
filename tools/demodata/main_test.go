package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaas/internal/server"
	"icaas/internal/store"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	site := filepath.Join("..", "..", "content", "site.json")
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if err := run(dir, site, now); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.List("submissions")
	if err != nil {
		t.Fatal(err)
	}
	subs, err := store.Decode[server.Submission](raw)
	if err != nil || len(subs) != len(proposals) {
		t.Fatalf("decoded %d of %d proposals: %v", len(subs), len(proposals), err)
	}
	if !strings.HasSuffix(subs[0].ID, "-W001") || !strings.HasSuffix(subs[1].ID, "-P001") {
		t.Errorf("first IDs are %s and %s", subs[0].ID, subs[1].ID)
	}
	var last time.Time
	for _, s := range subs {
		if !s.Received.After(last) || !s.Received.Before(now) {
			t.Errorf("%s received at %v: out of order or not in the past", s.ID, s.Received)
		}
		last = s.Received
		data, err := os.ReadFile(filepath.Join(dir, "papers", s.PaperFile))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if s.PaperFile != s.ID+".pdf" || s.PaperSize != int64(len(data)) || s.PaperSHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: the recorded paper details do not match the file", s.ID)
		}
		checkPDF(t, s.ID, data)
		if bytes.Contains(data, []byte(pdfEscape(s.SpeakerName))) {
			t.Errorf("%s: the paper names its speaker, but reviewers must not see who wrote it", s.ID)
		}
	}
	for name, want := range map[string]int{"contacts": len(contacts), "subscribers": len(subscribers)} {
		if n, err := st.Count(name); err != nil || n != want {
			t.Errorf("%s: %d records, want %d (%v)", name, n, want, err)
		}
	}

	if err := run(dir, site, now); err == nil {
		t.Error("a second run wrote into a directory that already holds records")
	}
}

// checkPDF verifies the structure of a generated PDF: the header and
// trailer, that every cross-reference entry points at its object and that
// the content stream has the declared length.
func checkPDF(t *testing.T, id string, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(data, []byte("%%EOF\n")) {
		t.Fatalf("%s: not a PDF file", id)
	}
	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s: no startxref", id)
	}
	xref, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(data[xref:], []byte("xref\n")) {
		t.Fatalf("%s: startxref does not point at the cross-reference table", id)
	}
	entries := regexp.MustCompile(`(\d{10}) 00000 n \n`).FindAllSubmatch(data[xref:], -1)
	if len(entries) != 7 {
		t.Fatalf("%s: %d cross-reference entries, want 7", id, len(entries))
	}
	for i, e := range entries {
		off, _ := strconv.Atoi(string(e[1]))
		if want := fmt.Sprintf("%d 0 obj\n", i+1); !bytes.HasPrefix(data[off:], []byte(want)) {
			t.Errorf("%s: entry %d does not point at its object", id, i+1)
		}
	}
	lm := regexp.MustCompile(`/Length (\d+) >>\nstream\n`).FindSubmatchIndex(data)
	if lm == nil {
		t.Fatalf("%s: no content stream", id)
	}
	n, _ := strconv.Atoi(string(data[lm[2]:lm[3]]))
	if !bytes.HasPrefix(data[lm[1]+n:], []byte("\nendstream")) {
		t.Errorf("%s: the content stream does not have its declared length", id)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("one two three four five", 9)
	want := []string{"one two", "three", "four five"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	if got := pdfEscape(`a (b) c\d – é`); got != `a \(b\) c\\d - ?` {
		t.Errorf("pdfEscape = %q", got)
	}
}
