package media

import (
	"sort"
	"testing"
)

func TestNormalizeQuery(t *testing.T) {
	cases := map[string]string{
		"  Dune: Part Two  ":     "dune part two",
		"Spider-Man":             "spider man",
		"Spider Man":             "spider man",
		"Дюна   (2021)":          "дюна 2021",
		"THE\tMATRIX!!":          "the matrix",
		"Léon: The Professional": "léon the professional",
	}

	for input, want := range cases {
		if got := NormalizeQuery(input); got != want {
			t.Errorf("NormalizeQuery(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseQuality(t *testing.T) {
	cases := map[string]string{
		"Dune 2021 2160p UHD BluRay": Quality2160,
		"Dune 2021 4K HDR":           Quality2160,
		"Dune 2021 1080p BDRip":      Quality1080,
		"Dune 2021 FullHD":           Quality1080,
		"Dune 2021 720p WEB-DL":      Quality720,
		"Dune 2021 HDTV":             Quality720,
		"Dune 2021 DVDRip":           QualitySD,
		"Dune 2160p HDR10 x265":      Quality2160, // "hd" inside HDR must not win
		"Dune 1080p HDR":             Quality1080,
	}

	for title, want := range cases {
		if got := ParseQuality(title); got != want {
			t.Errorf("ParseQuality(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestParseCodec(t *testing.T) {
	cases := map[string]string{
		"Dune 2160p x265":       CodecH265,
		"Dune 2160p H.265 HEVC": CodecH265,
		"Dune 1080p x264":       CodecH264,
		"Dune 1080p H264 AVC":   CodecH264,
		"Dune 1080p XviD":       CodecOther,
	}

	for title, want := range cases {
		if got := ParseCodec(title); got != want {
			t.Errorf("ParseCodec(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestParseSizeBytes(t *testing.T) {
	cases := map[string]int64{
		"1.46 GB": 1<<30 + 494_236_303/1_000*0, // checked loosely below
		"700 MB":  700 << 20,
		"2,5 GB":  2*(1<<30) + (1<<30)/2,
		"1.5 ГБ":  1*(1<<30) + (1<<30)/2,
		"":        0,
		"n/a":     0,
	}

	for input, want := range cases {
		got := ParseSizeBytes(input)
		if input == "1.46 GB" {
			if got < 1_560_000_000 || got > 1_580_000_000 {
				t.Errorf("ParseSizeBytes(%q) = %d, want ~1.57e9", input, got)
			}
			continue
		}
		if got != want {
			t.Errorf("ParseSizeBytes(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestBetterOrdersByQualityThenCodecThenSize(t *testing.T) {
	candidates := []Ranked{
		{Attributes: Attributes{Quality: Quality720, Codec: CodecH265, SizeBytes: 9 << 30}},
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH264, SizeBytes: 1 << 30}},
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH265, SizeBytes: 1 << 30}},
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH265, SizeBytes: 8 << 30}},
		{Attributes: Attributes{Quality: Quality1080, Codec: CodecH265, SizeBytes: 4 << 30}},
	}

	sort.SliceStable(candidates, func(i, j int) bool { return Better(candidates[i], candidates[j]) })

	want := []Ranked{
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH265, SizeBytes: 8 << 30}},
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH265, SizeBytes: 1 << 30}},
		{Attributes: Attributes{Quality: Quality2160, Codec: CodecH264, SizeBytes: 1 << 30}},
		{Attributes: Attributes{Quality: Quality1080, Codec: CodecH265, SizeBytes: 4 << 30}},
		{Attributes: Attributes{Quality: Quality720, Codec: CodecH265, SizeBytes: 9 << 30}},
	}

	for i := range want {
		if candidates[i] != want[i] {
			t.Fatalf("position %d = %+v, want %+v", i, candidates[i], want[i])
		}
	}
}

func TestTrackerPriorityBeatsQuality(t *testing.T) {
	preferred := Ranked{Priority: 1, Attributes: Attributes{Quality: Quality720, Codec: CodecH264}}
	other := Ranked{Priority: 2, Attributes: Attributes{Quality: Quality2160, Codec: CodecH265}}

	if !Better(preferred, other) {
		t.Error("tracker priority must outrank quality")
	}
}

func TestFilename(t *testing.T) {
	got := Filename("Dune: Part Two (2024)", "mazepa", Quality2160, "01JQ8X4M7ZK3RN")
	// The trailing separator run is trimmed, so the name never ends in "_-".
	want := "Dune_ Part Two _2024-mazepa-2160p-01JQ8X4M7ZK3RN.torrent"
	if got != want {
		t.Errorf("Filename() = %q, want %q", got, want)
	}
}
