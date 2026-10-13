package execution

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestImageTileBudgetBoundaries(t *testing.T) {
	for _, tc := range []struct {
		w, h  int
		tiles int64
	}{{1, 1, 1}, {511, 511, 1}, {512, 512, 1}, {513, 512, 2}, {1, 2048, 4}, {1024, 1024, 4}, {2048, 2048, 16}} {
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, tc.w, tc.h))); err != nil {
			t.Fatal(err)
		}
		in := testCall("openai", "")
		in.MaxOutputTokens = 0 // Exercise the real default output reservation (3000).
		base, err := EstimateTokens(in)
		if err != nil {
			t.Fatal(err)
		}
		in.Attachments = []Attachment{{Name: "test.png", MIME: "image/png", Data: b.Bytes()}}
		n, err := EstimateTokens(in)
		if err != nil {
			t.Fatal(err)
		}
		if n-base != ImageTokenOverhead+tc.tiles*ImageTokensPerTile {
			t.Fatalf("%dx%d reservation=%d", tc.w, tc.h, n-base)
		}
		if n > 200000 {
			t.Fatalf("single supported %dx%d exceeds default TPM: %d", tc.w, tc.h, n)
		}
	}
}
