package lyric

// Unsupported optional metadata must not prevent RID-based lyric fetching.
import (
	"strings"
	"testing"
)

func TestRequestUnicodeMetadataFallsBackIndependently(t *testing.T) {
	for _, s := range []string{"Señorita", "Mötley Crüe", "한국어", "song🎵"} {
		for _, songInvalid := range []bool{false, true} {
			r := Request{RID: "123", Song: "title", Artist: "artist"}
			badKey, goodField := "artist", "&songname=title"
			if songInvalid {
				r.Song = s
				badKey, goodField = "songname", "&artist=artist"
			} else {
				r.Artist = s
			}
			plain, err := r.Plain()
			if err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			if strings.Contains(plain, "&"+badKey+"=") || !strings.Contains(plain, goodField) || !strings.Contains(plain, "&rid=MUSIC_123&lrcx=1&olrc=1") {
				t.Fatalf("optional field fallback damaged request: %q", plain)
			}
		}
	}
	r := Request{RID: "123", Song: "🎵", Artist: "한국어"}
	if p, err := r.Plain(); err != nil || strings.Contains(p, "&songname=") || strings.Contains(p, "&artist=") {
		t.Fatalf("RID-only: %q %v", p, err)
	}
	r.RID = "invalid"
	if _, err := r.PlainBytes(); err != ErrInvalidRID {
		t.Fatalf("RID validation lost: %v", err)
	}
}
