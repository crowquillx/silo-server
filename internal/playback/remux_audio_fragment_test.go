package playback

import (
	"context"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAudioOnlyRemuxProducesDecodableFragments(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "theme.wav")
	// A long audio-only source has no video keyframes to cut fragments on.
	// One large moof can exceed the muxer's pipe buffer and become corrupt.
	command := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=107", "-c:a", "pcm_s16le", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate audio: %v: %s", err, out)
	}
	req := httptest.NewRequest("GET", "/theme", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	err = ServeRemuxWithOptions(response, req, source, "mp4", 0, true, -1, 0, RemuxServeOptions{FFmpegPath: ffmpeg, AudioOnly: true, TargetAudioChannels: 2, TargetAudioBitrateKbps: 192})
	if err != nil || response.Code != 200 {
		t.Fatalf("convert: status=%d err=%v", response.Code, err)
	}
	data := response.Body.Bytes()
	fragments := 0
	for offset := 0; offset < len(data); {
		if len(data)-offset < 8 {
			t.Fatal("truncated MP4 box")
		}
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if size < 8 || size > len(data)-offset {
			t.Fatalf("invalid MP4 box at %d: size=%d", offset, size)
		}
		if string(data[offset+4:offset+8]) == "moof" {
			fragments++
		}
		offset += size
	}
	if fragments < 2 {
		t.Fatalf("audio streamed as %d unbounded fragments", fragments)
	}
	output := filepath.Join(dir, "theme.m4a")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	decode := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-xerror", "-i", output, "-f", "null", "-")
	if out, err := decode.CombinedOutput(); err != nil {
		t.Fatalf("converted audio does not decode: %v: %s", err, out)
	}
}
