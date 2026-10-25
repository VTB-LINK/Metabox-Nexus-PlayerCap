package lyric

// This file guards bounded responses, including compressed expansion.
import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseLimitsRawAndInflated(t *testing.T) {
	if _, err := DecodeResponse(make([]byte, maxLRCBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("raw size limit: %v", err)
	}
	var body bytes.Buffer
	body.WriteString("tp=content\r\n\r\n")
	zw := zlib.NewWriter(&body)
	_, _ = zw.Write(bytes.Repeat([]byte("A"), maxLRCBytes*2+1))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeResponse(body.Bytes()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("inflate size limit: %v", err)
	}
}

func TestResponseLimitsHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxLRCBytes+1))
	}))
	defer srv.Close()
	client := &Client{HTTP: srv.Client(), BaseURL: srv.URL + "?"}
	if _, err := client.Fetch(context.Background(), Request{RID: "123"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("HTTP body limit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Fetch(ctx, Request{RID: "123"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
}

func TestRequestRejectsInvalidRID(t *testing.T) {
	for _, rid := range []string{"", "MUSIC_", "12&req=2", "MUSIC_MUSIC_1", "-1", " 12", strings.Repeat("9", 21)} {
		if _, err := (Request{RID: rid}).PlainBytes(); !errors.Is(err, ErrInvalidRID) {
			t.Fatalf("rid=%q accepted: %v", rid, err)
		}
	}
}
