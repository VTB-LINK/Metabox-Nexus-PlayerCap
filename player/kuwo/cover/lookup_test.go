package cover

// These tests guard the RID-to-cover protocol and prevent failed/stale lookups
// from being downloaded. They do not require Kuwo or an external service.
import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

func TestLookupRequestAndDownload(t *testing.T) {
	const artwork = "http://img1.kwcdn.kuwo.cn/star/albumcover/500/s4s67/92/1349231143.jpg"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Encode() != "pictype=url&rid=568213041&size=500&type=rid_pic" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, artwork+"\r\n")
	}))
	defer server.Close()
	for _, rid := range []string{"MUSIC_568213041", "568213041"} {
		got, err := fetch(context.Background(), rid, time.Second, server.URL, func(name, u string, timeout time.Duration) string {
			if name != "kuwo" || u != artwork || timeout <= 0 || timeout > time.Second {
				t.Errorf("bad download arguments: %s %s %s", name, u, timeout)
			}
			return "normalized image"
		})
		if err != nil || got.RID != "MUSIC_568213041" || got.URL != artwork || got.DataURI != "normalized image" {
			t.Fatalf("result=%+v err=%v", got, err)
		}
	}
}

func TestFailedLookupNeverDownloads(t *testing.T) {
	for _, body := range []string{"NO_PIC", "", "<html>bad gateway</html>", "file:///a.jpg", "http://localhost/a.jpg", "http://kuwo.cn.evil.test/a.jpg", "http://user@img1.kuwo.cn/a.jpg", "http://img1.kuwo.cn/a.jpg#fragment", "http://img1.kuwo.cn:8080/a.jpg", strings.Repeat("x", 4097)} {
		t.Run(fmt.Sprintf("body_%d_%s", len(body), strings.ReplaceAll(body, "/", "_")), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			_, err := fetch(context.Background(), "MUSIC_568213041", time.Second, server.URL, func(string, string, time.Duration) string {
				t.Fatal("download after failed lookup")
				return ""
			})
			if err == nil {
				t.Fatal("accepted bad response")
			}
			if (body == "NO_PIC" || body == "") && !errors.Is(err, ErrNoCover) {
				t.Fatalf("expected ErrNoCover, got %v", err)
			}
		})
	}
}

func TestInvalidRIDNeverRequests(t *testing.T) {
	for _, rid := range []string{"", "0", "MUSIC_0", "MUSIC_", "568213041&rid=1", "-12", " MUSIC_12", "123456789012345678901"} {
		_, err := fetch(context.Background(), rid, time.Second, ":invalid", func(string, string, time.Duration) string {
			t.Fatal("invalid RID was downloaded")
			return ""
		})
		if err == nil || err.Error() != i18n.T("kuwo: invalid cover RID") {
			t.Errorf("RID %q: %v", rid, err)
		}
	}
}

func TestCancelledCoverResultDiscarded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "https://img1.kuwo.cn/a.jpg") }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := fetch(ctx, "568213041", time.Second, server.URL, func(string, string, time.Duration) string {
		cancel()
		return "stale image"
	})
	if !errors.Is(err, context.Canceled) || got.DataURI != "" {
		t.Fatalf("stale result leaked: %+v %v", got, err)
	}
}

func TestLookupRedirectNotFollowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			t.Error("unexpected request to redirected endpoint")
			fmt.Fprint(w, "https://img1.kuwo.cn/a.jpg")
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	_, err := fetch(context.Background(), "568213041", time.Second, server.URL, func(string, string, time.Duration) string {
		t.Fatal("download after redirected lookup")
		return ""
	})
	if err == nil {
		t.Fatal("accepted redirected lookup")
	}
}

func TestCancelledLookupNeverDownloads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request after cancellation")
	}))
	defer server.Close()
	got, err := fetch(ctx, "568213041", time.Second, server.URL, func(string, string, time.Duration) string {
		t.Fatal("download after cancellation")
		return ""
	})
	if !errors.Is(err, context.Canceled) || got.DataURI != "" {
		t.Fatalf("cancelled lookup: %+v %v", got, err)
	}
}
