package upload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogoga_dictionary/internal/apperr"
)

var (
	jpegBytes = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'}
	pngBytes  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D}
	webpBytes = []byte("RIFF\x10\x00\x00\x00WEBPVP8 ")
	heicBytes = []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00")
	mif1Bytes = []byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00")
)

func TestDetectImage(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		ct   string
		ext  string
		ok   bool
	}{
		{"jpeg", jpegBytes, "image/jpeg", ".jpg", true},
		{"png", pngBytes, "image/png", ".png", true},
		{"webp", webpBytes, "image/webp", ".webp", true},
		{"heic", heicBytes, "image/heic", ".heic", true},
		{"heif", mif1Bytes, "image/heif", ".heif", true},
		{"gif", []byte("GIF89a......"), "", "", false},
		{"html", []byte("<html><script>"), "", "", false},
		{"mp4", []byte("\x00\x00\x00\x18ftypisom\x00\x00"), "", "", false},
		{"short", []byte{0xFF, 0xD8}, "", "", false},
	}
	for _, tc := range cases {
		img, ok := DetectImage(tc.in)
		if ok != tc.ok || img.ContentType != tc.ct || img.Ext != tc.ext {
			t.Errorf("%s: got %+v %v", tc.name, img, ok)
		}
	}
}

func TestNormalizeKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"feedback/bug":                        "feedback/bug",
		" feedback/ui_issue ":                 "feedback/ui_issue",
		"feedback/bug/":                       "feedback/bug",
		"feedback/a-b_c9":                     "feedback/a-b_c9",
		"":                                    DefaultKeyPrefix,
		"feedback":                            DefaultKeyPrefix,
		"feedback/Bug":                        DefaultKeyPrefix,
		"feedback/../secret":                  DefaultKeyPrefix,
		"feedback/a/b":                        DefaultKeyPrefix,
		"other/bug":                           DefaultKeyPrefix,
		"feedback/" + strings.Repeat("a", 33): DefaultKeyPrefix,
	}
	for in, want := range cases {
		if got := NormalizeKeyPrefix(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestUploadImageSniffsAndSendsPolicy(t *testing.T) {
	var gotKey, gotCT string
	var policy map[string]any
	qiniu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		gotKey = r.FormValue("key")
		parts := strings.Split(r.FormValue("token"), ":")
		raw, _ := base64.URLEncoding.DecodeString(parts[2])
		_ = json.Unmarshal(raw, &policy)
		f, h, _ := r.FormFile("file")
		gotCT = h.Header.Get("Content-Type")
		io.Copy(io.Discard, f)
		io.WriteString(w, `{"key":"`+gotKey+`"}`)
	}))
	defer qiniu.Close()

	svc, err := NewQiniuService(Config{AccessKey: "ak", SecretKey: "sk", Bucket: "b", UploadURL: qiniu.URL, PublicBaseURL: "https://cdn.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadImage(context.Background(), pngBytes, "feedback/../../x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotKey, "feedback/other/") || !strings.HasSuffix(gotKey, ".png") || gotCT != "image/png" {
		t.Fatalf("key=%q ct=%q", gotKey, gotCT)
	}
	if res.URL != "https://cdn.example.com/"+gotKey {
		t.Fatalf("url = %q", res.URL)
	}
	if policy["fsizeLimit"] != float64(MaxImageBytes) || policy["detectMime"] != float64(1) || policy["mimeLimit"] != allowedMimeLimit {
		t.Fatalf("policy = %v", policy)
	}
	if policy["scope"] != "b:"+gotKey {
		t.Fatalf("scope = %v", policy["scope"])
	}

	_, err = svc.UploadImage(context.Background(), []byte("<svg onload=alert(1)>"), "feedback/bug")
	if !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := apperr.AsValidation(err); !ok {
		t.Fatal("unsupported image should be a validation error")
	}
}
