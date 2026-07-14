package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUniqueSlug(t *testing.T) {
	s := &Store{}
	day := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		caption string
		want    string
	}{
		{"sat on the clean laundry again", "2026-07-13-sat-on-the-clean-laundry-again"},
		{"sat on the clean laundry again, twice", "2026-07-13-sat-on-the-clean-laundry-again"}, // capped at 6 words
		{"", "2026-07-13"},
		{"!!! ???", "2026-07-13"},
		{"Ärger mit KÄSE", "2026-07-13-rger-mit-k-se"},
		{"<script>alert(1)</script>", "2026-07-13-script-alert-1-script"},
	}
	for _, tc := range tests {
		if got := s.uniqueSlug(day, tc.caption); got != tc.want {
			t.Errorf("uniqueSlug(%q) = %q, want %q", tc.caption, got, tc.want)
		}
	}
}

func TestUniqueSlugCollides(t *testing.T) {
	s := &Store{posts: []Post{{ID: "2026-07-13-nap"}, {ID: "2026-07-13-nap-2"}}}
	day := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	if got := s.uniqueSlug(day, "nap"); got != "2026-07-13-nap-3" {
		t.Errorf("got %q, want 2026-07-13-nap-3", got)
	}
}

func TestMediaNameRejectsTraversal(t *testing.T) {
	good := []string{
		"a2e0ba5656b50c60130692f8.jpg",
		"a2e0ba5656b50c60130692f8-t.jpg",
	}
	bad := []string{
		"../posts.json",
		"../../etc/passwd",
		"a2e0ba5656b50c60130692f8.jpg/../../posts.json",
		"posts.json",
		"a2e0ba5656b50c60130692f8.php",
		"A2E0BA5656B50C60130692F8.jpg", // uppercase is not what we generate
		"",
	}
	for _, n := range good {
		if !mediaName.MatchString(n) {
			t.Errorf("rejected a legitimate name: %q", n)
		}
	}
	for _, n := range bad {
		if mediaName.MatchString(n) {
			t.Errorf("accepted a dangerous name: %q", n)
		}
	}
}

func TestExifOrientation(t *testing.T) {
	// A minimal JPEG carrying an APP1/EXIF block whose Orientation tag is 6.
	exif := []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xE1, 0x00, 0x1E, // APP1, length 30
		'E', 'x', 'i', 'f', 0x00, 0x00,
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, // TIFF header, little endian, IFD at 8
		0x01, 0x00, // one entry
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00, // tag 0x0112 = 6
		0x00, 0x00, 0x00, 0x00,
	}
	if got := exifOrientation(exif); got != 6 {
		t.Errorf("orientation = %d, want 6", got)
	}
	if got := exifOrientation([]byte{0xFF, 0xD8, 0xFF, 0xD9}); got != 1 {
		t.Errorf("orientation of a plain jpeg = %d, want 1", got)
	}
	if got := exifOrientation([]byte("not a jpeg at all")); got != 1 {
		t.Errorf("orientation of junk = %d, want 1", got)
	}
	// Truncated EXIF must not panic or read out of bounds.
	for i := range exif {
		_ = exifOrientation(exif[:i])
	}
}

func TestApplyOrientationRotates(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2)) // wide
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})    // top-left marker

	got := applyOrientation(src, 6) // rotate 90° clockwise
	b := got.Bounds()
	if b.Dx() != 2 || b.Dy() != 4 {
		t.Fatalf("size after rotation = %dx%d, want 2x4", b.Dx(), b.Dy())
	}
	// The top-left pixel should now be in the top-right corner.
	if r, _, _, _ := got.At(1, 0).RGBA(); r>>8 != 255 {
		t.Error("the marker pixel did not land in the top-right corner")
	}
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessUpload(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"media", "originals"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	p, err := processUpload(dir, testJPEG(t, 4000, 2000))
	if err != nil {
		t.Fatalf("processUpload: %v", err)
	}
	if p.Width != displayMaxSide || p.Height != displayMaxSide/2 {
		t.Errorf("display size = %dx%d, want %dx%d", p.Width, p.Height, displayMaxSide, displayMaxSide/2)
	}
	if !mediaName.MatchString(p.Image) || !mediaName.MatchString(p.Thumb) {
		t.Errorf("generated a filename the media route would reject: %q / %q", p.Image, p.Thumb)
	}
	for _, f := range []string{
		filepath.Join(dir, "media", p.Image),
		filepath.Join(dir, "media", p.Thumb),
		filepath.Join(dir, "originals", p.Original),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected file on disk: %v", err)
		}
	}
}

func TestProcessUploadStripsEXIF(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"media", "originals"} {
		os.MkdirAll(filepath.Join(dir, d), 0o750)
	}

	// A JPEG whose EXIF says "taken at these GPS coordinates". After
	// re-encoding, the served file must not contain it.
	raw := testJPEG(t, 100, 100)
	withExif := append([]byte{
		0xFF, 0xD8,
		0xFF, 0xE1, 0x00, 0x1E,
		'E', 'x', 'i', 'f', 0x00, 0x00,
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00,
		0x01, 0x00,
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, raw[2:]...)

	p, err := processUpload(dir, withExif)
	if err != nil {
		t.Fatalf("processUpload: %v", err)
	}
	served, err := os.ReadFile(filepath.Join(dir, "media", p.Image))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(served, []byte("Exif")) {
		t.Error("the served image still carries an EXIF block")
	}
}

func TestProcessUploadRejectsNonImages(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"media", "originals"} {
		os.MkdirAll(filepath.Join(dir, d), 0o750)
	}

	for name, body := range map[string][]byte{
		"empty":  {},
		"script": []byte(`<?php system($_GET["c"]); ?>`),
		"html":   []byte("<html><script>alert(1)</script>"),
		"random": []byte("\x00\x01\x02\x03not an image"),
	} {
		if _, err := processUpload(dir, body); err == nil {
			t.Errorf("%s: accepted a file that is not an image", name)
		}
	}
}

func TestFitNeverUpscales(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 200, 100))
	got := fit(small, displayMaxSide).Bounds()
	if got.Dx() != 200 || got.Dy() != 100 {
		t.Errorf("a small image was upscaled to %dx%d", got.Dx(), got.Dy())
	}
}

func TestLimiterBlocksAfterFive(t *testing.T) {
	l := newLimiter()
	for i := 0; i < loginMaxPerIP; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("attempt %d was blocked too early", i+1)
		}
	}
	if l.allow("10.0.0.1") {
		t.Error("the 6th attempt should have been blocked")
	}
	if !l.allow("10.0.0.2") {
		t.Error("a different IP should not be affected")
	}
	l.succeed("10.0.0.1")
	if !l.allow("10.0.0.1") {
		t.Error("a successful login should clear the strikes")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newSessionStore()
	tok, csrf := s.create()

	got, ok := s.get(tok)
	if !ok || got.csrf != csrf {
		t.Fatal("a fresh session should be retrievable")
	}
	if !checkCSRF(got, csrf) || checkCSRF(got, "wrong") {
		t.Error("csrf comparison is broken")
	}
	s.destroy(tok)
	if _, ok := s.get(tok); ok {
		t.Error("a destroyed session should be gone")
	}
	if _, ok := s.get(""); ok {
		t.Error("an empty token should never validate")
	}
}
