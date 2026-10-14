package tts

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleflightCollapsesConcurrentMisses(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
		fmt.Fprintf(w, `{"code":0,"data":%q}{"code":20000000}`, base64.StdEncoding.EncodeToString([]byte("Z")))
	}))
	defer upstream.Close()
	svc := newTestService(t, upstream.URL)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			audio, _, err := svc.Synthesize(context.Background(), Request{Text: "same", Speaker: "voice_a"})
			if err != nil || string(audio) != "Z" {
				t.Errorf("audio=%q err=%v", audio, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestUpstreamCallSurvivesClientCancel(t *testing.T) {
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		if r.Context().Err() != nil {
			t.Error("upstream request was cancelled")
		}
		fmt.Fprintf(w, `{"code":0,"data":%q}{"code":20000000}`, base64.StdEncoding.EncodeToString([]byte("Q")))
		close(done)
	}))
	defer upstream.Close()
	svc := newTestService(t, upstream.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, _, err := svc.Synthesize(ctx, Request{Text: "x", Speaker: "voice_a"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	<-done
	// Result was cached even though the client went away.
	if _, cached, _ := svc.Synthesize(context.Background(), Request{Text: "x", Speaker: "voice_a"}); !cached {
		t.Fatal("expected cache hit")
	}
}

func TestSweepCacheRemovesOldest(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(Config{APIKey: "k", CacheDir: dir, Speakers: []string{"v"}, CacheMaxBytes: 250})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "ab", fmt.Sprintf("f%d.mp3", i))
		if err := writeFileAtomic(p, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	info, _ := os.Stat(filepath.Join(dir, "ab", "f0.mp3"))
	if perm := info.Mode().Perm(); perm != cacheFilePerm {
		t.Fatalf("file perm = %o", perm)
	}
	removed, err := svc.SweepCache()
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	for i, want := range []bool{false, false, false, true, true} {
		_, err := os.Stat(filepath.Join(dir, "ab", fmt.Sprintf("f%d.mp3", i)))
		if (err == nil) != want {
			t.Errorf("f%d exists=%v want %v", i, err == nil, want)
		}
	}
}
